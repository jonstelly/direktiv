package direktiv

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/vorteil/direktiv/ent/workflowinstance"
	"github.com/vorteil/direktiv/pkg/dlog/dummy"
	"github.com/vorteil/direktiv/pkg/ingress"

	"github.com/jinzhu/copier"
	"github.com/vorteil/direktiv/pkg/flow"
	"github.com/vorteil/direktiv/pkg/isolate"
	"google.golang.org/grpc"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/google/uuid"
	"github.com/itchyny/gojq"
	"github.com/mitchellh/hashstructure/v2"
	"github.com/senseyeio/duration"
	log "github.com/sirupsen/logrus"
	"github.com/vorteil/direktiv/ent"
	"github.com/vorteil/direktiv/pkg/dlog"
	"github.com/vorteil/direktiv/pkg/model"
)

const (
	// WorkflowStateSubscription is the channel that runs workflow states.
	WorkflowStateSubscription = "workflow-state"
)

var (
	ErrCodeJQBadQuery        = "direktiv.jq.badCommand"
	ErrCodeJQNotObject       = "direktiv.jq.notObject"
	ErrCodeMultipleErrors    = "direktiv.workflow.multipleErrors"
	ErrCodeAllBranchesFailed = "direktiv.parallel.allFailed"
)

type workflowEngine struct {
	db             *dbManager
	grpcFlow       flow.DirektivFlowClient
	grpcIngress    ingress.DirektivIngressClient
	grpcIsolate    isolate.DirektivIsolateClient
	timer          *timerManager
	instanceLogger *dlog.Log
	stateLogics    map[model.StateType]func(*model.Workflow, model.State) (stateLogic, error)
	server         *WorkflowServer

	cancels     map[string]func()
	cancelsLock sync.Mutex
}

func newWorkflowEngine(s *WorkflowServer) (*workflowEngine, error) {

	var err error

	we := new(workflowEngine)
	we.server = s
	we.db = s.dbManager
	we.timer = s.tmManager
	we.instanceLogger = &s.instanceLogger
	we.cancels = make(map[string]func())

	opts := []grpc.DialOption{grpc.WithInsecure()}

	conn, err := grpc.Dial(s.config.IngressAPI.Endpoint, opts...)
	if err != nil {
		return nil, err
	}

	we.grpcIngress = ingress.NewDirektivIngressClient(conn)

	conn, err = grpc.Dial(s.config.FlowAPI.Endpoint, opts...)
	if err != nil {
		return nil, err
	}

	// TODO: close conn
	// TODO: address configuration

	we.grpcFlow = flow.NewDirektivFlowClient(conn)

	conn, err = grpc.Dial(s.config.IsolateAPI.Endpoint, opts...)
	if err != nil {
		return nil, err
	}

	// TODO: close conn
	// TODO: address configuration

	we.grpcIsolate = isolate.NewDirektivIsolateClient(conn)

	we.stateLogics = map[model.StateType]func(*model.Workflow, model.State) (stateLogic, error){
		model.StateTypeNoop:          initNoopStateLogic,
		model.StateTypeAction:        initActionStateLogic,
		model.StateTypeConsumeEvent:  initConsumeEventStateLogic,
		model.StateTypeDelay:         initDelayStateLogic,
		model.StateTypeError:         initErrorStateLogic,
		model.StateTypeEventsAnd:     initEventsAndStateLogic,
		model.StateTypeEventsXor:     initEventsXorStateLogic,
		model.StateTypeForEach:       initForEachStateLogic,
		model.StateTypeGenerateEvent: initGenerateEventStateLogic,
		model.StateTypeParallel:      initParallelStateLogic,
		model.StateTypeSwitch:        initSwitchStateLogic,
		model.StateTypeValidate:      initValidateStateLogic,
	}

	err = we.timer.registerFunction(sleepWakeupFunction, we.sleepWakeup)
	if err != nil {
		return nil, err
	}

	err = we.timer.registerFunction(retryWakeupFunction, we.retryWakeup)
	if err != nil {
		return nil, err
	}

	err = we.timer.registerFunction(timeoutFunction, we.timeoutHandler)
	if err != nil {
		return nil, err
	}

	err = we.timer.registerFunction(wfCron, we.wfCronHandler)
	if err != nil {
		return nil, err
	}

	return we, nil

}

func (we *workflowEngine) localCancel(id string) {

	we.timer.actionTimerByName(id, deleteTimerAction)
	we.cancelsLock.Lock()
	cancel, exists := we.cancels[id]
	if exists {
		delete(we.cancels, id)
		defer cancel()
	}
	we.cancelsLock.Unlock()

}

func (we *workflowEngine) finishCancelSubflow(id string) {
	we.localCancel(id)
}

type runStateMessage struct {
	InstanceID string
	State      string
	Step       int
}

func (we *workflowEngine) dispatchState(id, state string, step int) error {

	ctx := context.Background()

	// TODO: timeouts & retries

	var step32 int32
	step32 = int32(step)

	_, err := we.grpcFlow.Resume(ctx, &flow.ResumeRequest{
		InstanceId: &id,
		Step:       &step32,
	})
	if err != nil {
		return err
	}

	return nil

}

type eventsWaiterSignature struct {
	InstanceID string
	Step       int
}

type eventsResultMessage struct {
	InstanceID string
	State      string
	Step       int
	Payloads   []*cloudevents.Event
}

const eventsWakeupFunction = "eventsWakeup"

func (we *workflowEngine) wakeEventsWaiter(signature []byte, events []*cloudevents.Event) error {

	sig := new(eventsWaiterSignature)
	err := json.Unmarshal(signature, sig)
	if err != nil {
		return NewInternalError(err)
	}

	ctx, wli, err := we.loadWorkflowLogicInstance(sig.InstanceID, sig.Step)
	if err != nil {
		err = fmt.Errorf("cannot load workflow logic instance: %v", err)
		log.Error(err)
		return err
	}

	wakedata, err := json.Marshal(events)
	if err != nil {
		wli.Close()
		err = fmt.Errorf("cannot marshal the action results payload: %v", err)
		log.Error(err)
		return err
	}

	var savedata []byte

	if wli.rec.Memory != "" {

		savedata, err = base64.StdEncoding.DecodeString(wli.rec.Memory)
		if err != nil {
			wli.Close()
			err = fmt.Errorf("cannot decode the savedata: %v", err)
			log.Error(err)
			return err
		}

		// TODO: ?
		// wli.rec, err = wli.rec.Update().SetNillableMemory(nil).Save(ctx)
		// if err != nil {
		// 	log.Errorf("cannot update savedata information: %v", err)
		// 	return
		// }

	}

	go wli.engine.runState(ctx, wli, savedata, wakedata)

	return nil

}

type actionResultPayload struct {
	ActionID     string
	ErrorCode    string
	ErrorMessage string
	Output       []byte
}

type actionResultMessage struct {
	InstanceID string
	State      string
	Step       int
	Payload    actionResultPayload
}

func (we *workflowEngine) doActionRequest(ctx context.Context, ar *actionRequest) error {

	// TODO: should this ctx be modified with a shorter deadline?

	var step int32
	step = int32(ar.Workflow.Step)

	var timeout int64
	timeout = int64(ar.Workflow.Timeout)

	_, err := we.grpcIsolate.RunIsolate(ctx, &isolate.RunIsolateRequest{
		ActionId:   &ar.ActionID,
		Namespace:  &ar.Workflow.Namespace,
		InstanceId: &ar.Workflow.InstanceID,
		Step:       &step,
		Timeout:    &timeout,
		Image:      &ar.Container.Image,
		Command:    &ar.Container.Cmd,
		Size:       &ar.Container.Size,
		Data:       ar.Container.Data,
		Registries: ar.Container.Registries,
	})
	if err != nil {
		return NewInternalError(err)
	}

	return nil

}

const actionWakeupFunction = "actionWakeup"

func (we *workflowEngine) wakeCaller(msg *actionResultMessage) error {

	ctx := context.Background()

	// TODO: timeouts & retries

	var step int32
	step = int32(msg.Step)

	_, err := we.grpcFlow.ReportActionResults(ctx, &flow.ReportActionResultsRequest{
		InstanceId:   &msg.InstanceID,
		Step:         &step,
		ActionId:     &msg.Payload.ActionID,
		ErrorCode:    &msg.Payload.ErrorCode,
		ErrorMessage: &msg.Payload.ErrorMessage,
		Output:       msg.Payload.Output,
	})
	if err != nil {
		return err
	}

	return nil

}

func (wli *workflowLogicInstance) Raise(ctx context.Context, cerr *CatchableError) error {

	var err error

	if wli.rec.ErrorCode != "" {
		wli.rec, err = wli.rec.Update().
			SetStatus("failed").
			SetErrorCode(cerr.Code).
			SetErrorMessage(cerr.Message).
			Save(ctx)
		if err != nil {
			return NewInternalError(err)
		}
	} else {
		return NewCatchableError(ErrCodeMultipleErrors, "the workflow instance tried to throw multiple errors")
	}

	return nil

}

const wfCron = "wfcron"

func (we *workflowEngine) wfCronHandler(data []byte) error {

	return we.CronInvoke(string(data))

}

type sleepMessage struct {
	InstanceID string
	State      string
	Step       int
}

const sleepWakeupFunction = "sleepWakeup"
const sleepWakedata = "sleep"

func (we *workflowEngine) sleep(id, state string, step int, t time.Time) error {

	data, err := json.Marshal(&sleepMessage{
		InstanceID: id,
		State:      state,
		Step:       step,
	})
	if err != nil {
		return NewInternalError(err)
	}

	_, err = we.timer.addOneShot(id, sleepWakeupFunction, t, data)
	if err != nil {
		return NewInternalError(err)
	}

	return nil

}

func (we *workflowEngine) sleepWakeup(data []byte) error {

	msg := new(sleepMessage)

	err := json.Unmarshal(data, msg)
	if err != nil {
		log.Errorf("cannot handle sleep wakeup: %v", err)
		return nil
	}

	ctx, wli, err := we.loadWorkflowLogicInstance(msg.InstanceID, msg.Step)
	if err != nil {
		log.Errorf("cannot load workflow logic instance: %v", err)
		return nil
	}

	wli.Log("Waking up from sleep.")

	go wli.engine.runState(ctx, wli, nil, []byte(sleepWakedata))

	return nil

}

func (we *workflowEngine) cancelChildren(rec *ent.WorkflowInstance) error {

	wfrec, err := rec.QueryWorkflow().Only(context.Background())
	if err != nil {
		return err
	}

	wf := new(model.Workflow)
	err = wf.Load(wfrec.Workflow)
	if err != nil {
		return err
	}

	step := len(rec.Flow)
	state := rec.Flow[step-1]
	states := wf.GetStatesMap()
	stateObject, exists := states[state]
	if !exists {
		return NewInternalError(fmt.Errorf("workflow cannot resolve state: %s", state))
	}

	init, exists := we.stateLogics[stateObject.GetType()]
	if !exists {
		return NewInternalError(fmt.Errorf("engine cannot resolve state type: %s", stateObject.GetType().String()))
	}

	stateLogic, err := init(wf, stateObject)
	if err != nil {
		return NewInternalError(fmt.Errorf("cannot initialize state logic: %v", err))
	}
	logic := stateLogic

	children := logic.LivingChildren([]byte(rec.Memory))
	for _, child := range children {
		switch child.Type {
		case "isolate":
			syncServer(context.Background(), we.db, &we.server.id, child.Id, cancelIsolate)
		case "subflow":
			go func(id string) {
				we.hardCancelInstance(id, "direktiv.cancels.parent", "cancelled by parent workflow")
			}(child.Id)
		default:
			log.Errorf("unrecognized child type: %s", child.Type)
		}
	}

	return nil

}

func (we *workflowEngine) hardCancelInstance(instanceId, code, message string) error {
	return we.cancelInstance(instanceId, code, message, false)
}

func (we *workflowEngine) softCancelInstance(instanceId string, step int, code, message string) error {
	// TODO: step
	return we.cancelInstance(instanceId, code, message, true)
}

func (we *workflowEngine) cancelInstance(instanceId, code, message string, soft bool) error {

	killer := make(chan bool)

	go func() {

		timer := time.After(time.Millisecond)

		for {

			select {
			case <-timer:
				// broadcast cancel across cluster
				syncServer(context.Background(), we.db, &we.server.id, instanceId, cancelSubflow)
				// TODO: mark cancelled instances even if not scheduled in
			case <-killer:
				return
			}

		}

	}()

	defer func() {
		close(killer)
	}()

	tx, err := we.db.dbEnt.Tx(context.Background())
	if err != nil {
		return err
	}

	rec, err := tx.WorkflowInstance.Query().Where(workflowinstance.InstanceIDEQ(instanceId)).Only(context.Background())
	if err != nil {
		return rollback(tx, err)
	}

	if rec.Status != "pending" && rec.Status != "running" {
		return rollback(tx, nil)
	}

	ns, err := rec.QueryWorkflow().QueryNamespace().Only(context.Background())
	if err != nil {
		return rollback(tx, err)
	}

	rec, err = rec.Update().
		SetStatus("cancelled").
		SetEndTime(time.Now()).
		SetErrorCode(code).
		SetErrorMessage(message).
		Save(context.Background())
	if err != nil {
		return rollback(tx, err)
	}

	err = tx.Commit()
	if err != nil {
		return rollback(tx, err)
	}

	err = we.cancelChildren(rec)
	if err != nil {
		log.Error(err)
	}

	we.timer.actionTimerByName(instanceId, deleteTimerAction)
	// TODO: cancel any other outstanding timers

	logger, err := (*we.instanceLogger).LoggerFunc(ns.ID, instanceId)
	if err != nil {
		dl, _ := dummy.NewLogger()
		logger, _ = dl.LoggerFunc(ns.ID, instanceId)
	}
	defer logger.Close()

	logger.Info(fmt.Sprintf("Workflow %s.", message))

	if rec.InvokedBy != "" {

		// wakeup caller
		caller := new(subflowCaller)
		err = json.Unmarshal([]byte(rec.InvokedBy), caller)
		if err != nil {
			log.Error(err)
			return nil
		}

		msg := &actionResultMessage{
			InstanceID: caller.InstanceID,
			State:      caller.State,
			Step:       caller.Step,
			Payload: actionResultPayload{
				ActionID:     instanceId,
				ErrorCode:    rec.ErrorCode,
				ErrorMessage: rec.ErrorMessage,
			},
		}

		logger.Info(fmt.Sprintf("Reporting failure to calling workflow."))

		err = we.wakeCaller(msg)
		if err != nil {
			log.Error(err)
			return nil
		}

	}

	return nil

}

type retryMessage struct {
	InstanceID string
	State      string
	Step       int
}

const retryWakeupFunction = "retryWakeup"

func (we *workflowEngine) scheduleRetry(id, state string, step int, t time.Time) error {

	data, err := json.Marshal(&retryMessage{
		InstanceID: id,
		State:      state,
		Step:       step,
	})
	if err != nil {
		return NewInternalError(err)
	}

	_, err = we.timer.addOneShot(id, retryWakeupFunction, t, data)
	if err != nil {
		return NewInternalError(err)
	}

	return nil

}

func (we *workflowEngine) retryWakeup(data []byte) error {

	msg := new(retryMessage)

	err := json.Unmarshal(data, msg)
	if err != nil {
		log.Errorf("cannot handle retry wakeup: %v", err)
		return nil
	}

	ctx, wli, err := we.loadWorkflowLogicInstance(msg.InstanceID, msg.Step)
	if err != nil {
		log.Errorf("cannot load workflow logic instance: %v", err)
		return nil
	}

	wli.Log("Retrying failed state.")

	go wli.engine.runState(ctx, wli, nil, nil)

	return nil

}

const maxWorkflowSteps = 10

func (we *workflowEngine) runState(ctx context.Context, wli *workflowLogicInstance, savedata, wakedata []byte) {

	log.Debugf("Running state logic -- %s:%v (%s)", wli.id, wli.step, wli.logic.ID())
	if len(savedata) == 0 && len(wakedata) == 0 {
		wli.Log("Running state logic -- %s:%v (%s)", wli.logic.ID(), wli.step, wli.logic.Type())
	}

	defer wli.unlock()
	defer wli.Close()

	var err error
	var transition *stateTransition

	if wli.step > maxWorkflowSteps {
		err = NewUncatchableError("direktiv.limits.steps", "instance aborted for exceeding the maximum number of state executions (%d)", maxWorkflowSteps)
		goto failure
	}

	transition, err = wli.logic.Run(ctx, wli, savedata, wakedata)
	if err != nil {
		goto failure
	}

next:
	if transition != nil {

		if transition.Transform != "" && transition.Transform != "." {
			wli.Log("Transforming state data.")
		}

		err = wli.Transform(transition.Transform)
		if err != nil {
			goto failure
		}

		if transition.NextState == "" {

			var rec *ent.WorkflowInstance
			data, err := json.Marshal(wli.data)
			if err != nil {
				err = fmt.Errorf("engine cannot marshal state data for storage: %v", err)
				log.Error(err)
				return
			}

			rec, err = wli.rec.Update().SetOutput(string(data)).SetEndTime(time.Now()).SetStatus("complete").Save(ctx)
			if err != nil {
				log.Error(err)
				return
			}

			wli.rec = rec
			log.Debugf("Workflow instance completed: %s", wli.id)
			wli.Log("Workflow completed.")

			// delete timers for workflow
			// id := fmt.Sprintf("timeout:%s:%d", wli.id, wli.step)
			// getTimersForInstance
			del, err := wli.engine.timer.deleteTimersForInstance(wli.id)
			if err != nil {
				log.Error(err)
			}
			log.Debugf("deleted %d timers for instance %v", del, wli.id)

			if wli.rec.InvokedBy != "" {

				// wakeup caller
				caller := new(subflowCaller)
				err = json.Unmarshal([]byte(wli.rec.InvokedBy), caller)
				if err != nil {
					log.Error(err)
					return
				}

				msg := &actionResultMessage{
					InstanceID: caller.InstanceID,
					State:      caller.State,
					Step:       caller.Step,
					Payload: actionResultPayload{
						ActionID: wli.id,
						Output:   data,
					},
				}

				wli.Log("Reporting results to calling workflow.")

				err = wli.engine.wakeCaller(msg)
				if err != nil {
					log.Error(err)
					return
				}

			}

			return

		}

		wli.Log("Transitioning to next state: %s (%d).", transition.NextState, wli.step)

		go wli.Transition(transition.NextState, 0)

	}

	return

failure:

	var breaker int

	if breaker > 10 {
		err = NewInternalError(errors.New("somehow ended up in a catchable error loop"))
	}

	children := wli.logic.LivingChildren([]byte(wli.rec.Memory))
	for _, child := range children {
		switch child.Type {
		case "isolate":
			syncServer(context.Background(), wli.engine.db, &wli.engine.server.id, child.Id, cancelIsolate)
		case "subflow":
			go func(id string) {
				wli.engine.hardCancelInstance(id, "direktiv.cancels.parent", "cancelled by parent workflow")
			}(child.Id)
		default:
			log.Errorf("unrecognized child type: %s", child.Type)
		}
	}

	if uerr, ok := err.(*UncatchableError); ok {

		if wli.rec.ErrorCode == "" {
			wli.rec, err = wli.rec.Update().
				SetStatus("failed").
				SetEndTime(time.Now()).
				SetErrorCode(uerr.Code).
				SetErrorMessage(uerr.Message).
				Save(ctx)
			if err != nil {
				err = NewInternalError(err)
				goto failure
			}
		}

		wli.Log("Workflow failed with uncatchable error: %s", uerr.Message)

		if wli.rec.InvokedBy != "" {

			// wakeup caller
			caller := new(subflowCaller)
			err = json.Unmarshal([]byte(wli.rec.InvokedBy), caller)
			if err != nil {
				log.Error(err)
				return
			}

			msg := &actionResultMessage{
				InstanceID: caller.InstanceID,
				State:      caller.State,
				Step:       caller.Step,
				Payload: actionResultPayload{
					ActionID:     wli.id,
					ErrorCode:    wli.rec.ErrorCode,
					ErrorMessage: wli.rec.ErrorMessage,
				},
			}

			wli.Log("Reporting failure to calling workflow.")

			err = wli.engine.wakeCaller(msg)
			if err != nil {
				log.Error(err)
				return
			}

		}

	} else if cerr, ok := err.(*CatchableError); ok {

		for i, catch := range wli.logic.ErrorCatchers() {

			var matched bool

			// NOTE: this error should be checked in model validation
			matched, _ = regexp.MatchString(catch.Error, cerr.Code)

			if matched {

				wli.Log("State failed with error '%s': %s", cerr.Code, cerr.Message)
				wli.Log("Error caught by error definition %d: %s", i, catch.Error)

				if catch.Retry != nil {
					if wli.rec.Attempts < catch.Retry.MaxAttempts {
						err = wli.Retry(ctx, catch.Retry.Delay, catch.Retry.Multiplier)
						if err != nil {
							goto failure
						}
						return
					} else {
						wli.Log("Maximum retry attempts exceeded.")
					}
				}

				transition = &stateTransition{
					Transform: "",
					NextState: catch.Transition,
				}

				breaker++

				goto next

			}

		}

		if wli.rec.ErrorCode == "" {
			wli.rec, err = wli.rec.Update().
				SetStatus("failed").
				SetEndTime(time.Now()).
				SetErrorCode(cerr.Code).
				SetErrorMessage(cerr.Message).
				Save(ctx)
			if err != nil {
				err = NewInternalError(err)
				goto failure
			}
		}

		wli.Log("Workflow failed with uncaught error '%s': %s", cerr.Code, cerr.Message)

		if wli.rec.InvokedBy != "" {

			// wakeup caller
			caller := new(subflowCaller)
			err = json.Unmarshal([]byte(wli.rec.InvokedBy), caller)
			if err != nil {
				log.Error(err)
				return
			}

			msg := &actionResultMessage{
				InstanceID: caller.InstanceID,
				State:      caller.State,
				Step:       caller.Step,
				Payload: actionResultPayload{
					ActionID:     wli.id,
					ErrorCode:    wli.rec.ErrorCode,
					ErrorMessage: wli.rec.ErrorMessage,
				},
			}

			wli.Log("Reporting failure to calling workflow.")

			err = wli.engine.wakeCaller(msg)
			if err != nil {
				log.Error(err)
				return
			}

		}

	} else if ierr, ok := err.(*InternalError); ok {

		code := ""
		msg := "an internal error occurred"

		if wli != nil && wli.rec != nil {

			var err error

			if wli.rec.ErrorCode == "" {

				wli.rec, err = wli.rec.Update().
					SetStatus("crashed").
					SetEndTime(time.Now()).
					SetErrorCode(code).
					SetErrorMessage(msg).
					Save(ctx)

			}

			if err == nil {

				log.Errorf("Workflow failed with internal error: %s", ierr.Error())
				wli.Log("Workflow crashed due to an internal error.")

				if wli.rec.InvokedBy != "" {

					// wakeup caller
					caller := new(subflowCaller)
					err = json.Unmarshal([]byte(wli.rec.InvokedBy), caller)
					if err != nil {
						log.Error(err)
						return
					}

					msg := &actionResultMessage{
						InstanceID: caller.InstanceID,
						State:      caller.State,
						Step:       caller.Step,
						Payload: actionResultPayload{
							ActionID:     wli.id,
							ErrorCode:    wli.rec.ErrorCode,
							ErrorMessage: wli.rec.ErrorMessage,
						},
					}

					wli.Log("Reporting failure to calling workflow.")

					err = wli.engine.wakeCaller(msg)
					if err != nil {
						log.Error(err)
						return
					}

				}

				return

			}

		}

		log.Errorf("Workflow failed with internal error and the database couldn't be updated: %s", ierr.Error())

	} else {
		log.Errorf("Unwrapped error detected: %v", err)
	}

	return

}

var letters = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

func randSeq(n int) string {
	b := make([]rune, n)
	for i := range b {
		rint, err := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
		if err != nil {
			panic(err)
		}
		b[i] = letters[int(rint.Int64())]
	}
	return string(b)
}

func (we *workflowEngine) CronInvoke(uid string) error {

	var err error

	wf, err := we.db.getWorkflow(uid)
	if err != nil {
		return err
	}

	ns, err := wf.QueryNamespace().Only(context.Background())
	if err != nil {
		return nil
	}

	wli, err := we.newWorkflowLogicInstance(ns.ID, wf.Name, []byte("{}"))
	if err != nil {
		if _, ok := err.(*InternalError); ok {
			log.Errorf("Internal error on CronInvoke: %v", err)
			return errors.New("an internal error occurred")
		} else {
			return err
		}
	}
	defer wli.Close()

	if wli.wf.Start == nil || wli.wf.Start.GetType() != model.StartTypeScheduled {
		return fmt.Errorf("cannot cron invoke workflows with '%s' starts", wli.wf.Start.GetType())
	}

	wli.rec, err = we.db.addWorkflowInstance(ns.ID, wf.Name, wli.id, string(wli.startData))
	if err != nil {
		return NewInternalError(err)
	}

	start := wli.wf.GetStartState()

	wli.Log("Beginning workflow triggered by API.")

	go wli.Transition(start.GetID(), 0)

	return nil

}

func (we *workflowEngine) DirectInvoke(namespace, name string, input []byte) (string, error) {

	var err error

	wli, err := we.newWorkflowLogicInstance(namespace, name, input)
	if err != nil {
		if _, ok := err.(*InternalError); ok {
			log.Errorf("Internal error on DirectInvoke: %v", err)
			return "", errors.New("an internal error occurred")
		} else {
			return "", err
		}
	}
	defer wli.Close()

	if wli.wf.Start != nil && wli.wf.Start.GetType() != model.StartTypeDefault {
		return "", fmt.Errorf("cannot directly invoke workflows with '%s' starts", wli.wf.Start.GetType())
	}

	wli.rec, err = we.db.addWorkflowInstance(namespace, name, wli.id, string(wli.startData))
	if err != nil {
		return "", NewInternalError(err)
	}

	start := wli.wf.GetStartState()

	wli.Log("Beginning workflow triggered by API.")

	go wli.Transition(start.GetID(), 0)

	return wli.id, nil

}

func (we *workflowEngine) EventsInvoke(workflowID uuid.UUID, events ...*cloudevents.Event) {

	wf, err := we.db.getWorkflowByID(workflowID)
	if err != nil {
		log.Errorf("Internal error on EventsInvoke: %v", err)
		return
	}

	ns, err := wf.QueryNamespace().Only(we.db.ctx)
	if err != nil {
		log.Errorf("Internal error on EventsInvoke: %v", err)
		return
	}

	var input []byte
	m := make(map[string]interface{})
	for _, event := range events {

		if event == nil {
			continue
		}

		var x interface{}

		if event.DataContentType() == "application/json" || event.DataContentType() == "" {
			err = json.Unmarshal(event.Data(), &x)
			if err != nil {
				log.Errorf("Invalid json payload for event: %v", err)
				return
			}
		} else {
			x = base64.StdEncoding.EncodeToString(event.Data())
		}

		m[event.Type()] = x

	}

	input, err = json.Marshal(m)
	if err != nil {
		log.Errorf("Internal error on EventsInvoke: %v", err)
		return
	}

	namespace := ns.ID
	name := wf.Name

	wli, err := we.newWorkflowLogicInstance(namespace, name, input)
	if err != nil {
		log.Errorf("Internal error on EventsInvoke: %v", err)
		return
	}
	defer wli.Close()

	var stype model.StartType
	if wli.wf.Start != nil {
		stype = wli.wf.Start.GetType()
	}

	switch stype {
	case model.StartTypeEvent:
	case model.StartTypeEventsAnd:
	case model.StartTypeEventsXor:
	default:
		log.Errorf("cannot event invoke workflows with '%s' starts", stype)
		return
	}

	wli.rec, err = we.db.addWorkflowInstance(namespace, name, wli.id, string(wli.startData))
	if err != nil {
		log.Errorf("Internal error on EventsInvoke: %v", err)
		return
	}

	start := wli.wf.GetStartState()

	if len(events) == 1 {
		wli.Log("Beginning workflow triggered by event: %s", events[0].ID())
	} else {
		var ids = make([]string, len(events))
		for i := range events {
			ids[i] = events[i].ID()
		}
		wli.Log("Beginning workflow triggered by events: %v", ids)
	}

	go wli.Transition(start.GetID(), 0)

}

type subflowCaller struct {
	InstanceID string
	State      string
	Step       int
	Depth      int
}

const maxSubflowDepth = 5

func (we *workflowEngine) subflowInvoke(caller *subflowCaller, callersCaller, namespace, name string, input []byte) (string, error) {

	var err error

	if callersCaller != "" {
		cc := new(subflowCaller)
		err = json.Unmarshal([]byte(callersCaller), cc)
		if err != nil {
			log.Errorf("Internal error on subflowInvoke: %v", err)
			return "", errors.New("an internal error occurred")
		}

		caller.Depth = cc.Depth + 1
		if caller.Depth > maxSubflowDepth {
			err = NewUncatchableError("direktiv.limits.depth", "instance aborted for exceeding the maximum subflow depth (%d)", maxSubflowDepth)
			return "", err
		}
	}

	wli, err := we.newWorkflowLogicInstance(namespace, name, input)
	if err != nil {
		if _, ok := err.(*InternalError); ok {
			log.Errorf("Internal error on subflowInvoke: %v", err)
			return "", errors.New("an internal error occurred")
		} else {
			return "", err
		}
	}
	defer wli.Close()

	if wli.wf.Start != nil && wli.wf.Start.GetType() != model.StartTypeDefault {
		return "", fmt.Errorf("cannot subflow invoke workflows with '%s' starts", wli.wf.Start.GetType())
	}

	wli.rec, err = we.db.addWorkflowInstance(namespace, name, wli.id, string(wli.startData))
	if err != nil {
		return "", NewInternalError(err)
	}

	if caller != nil {

		var data []byte

		data, err = json.Marshal(caller)
		if err != nil {
			return "", NewInternalError(err)
		}

		wli.rec, err = wli.rec.Update().SetInvokedBy(string(data)).Save(context.Background())
		if err != nil {
			return "", NewInternalError(err)
		}

	}

	start := wli.wf.GetStartState()

	wli.Log("Beginning workflow triggered as subflow to caller: %s", caller.InstanceID)

	go wli.Transition(start.GetID(), 0)

	return wli.id, nil

}

type workflowLogicInstance struct {
	engine    *workflowEngine
	data      interface{}
	startData []byte
	wf        *model.Workflow
	rec       *ent.WorkflowInstance
	step      int

	namespace string
	id        string
	lockConn  *sql.Conn
	logic     stateLogic
	logger    dlog.Logger
}

func (wli *workflowLogicInstance) Close() error {
	return wli.logger.Close()
}

func (we *workflowEngine) newWorkflowLogicInstance(namespace, name string, input []byte) (*workflowLogicInstance, error) {

	var err error
	var inputData, stateData interface{}

	err = json.Unmarshal(input, &inputData)
	if err != nil {
		inputData = base64.StdEncoding.EncodeToString(input)
	}

	if _, ok := inputData.(map[string]interface{}); ok {
		stateData = inputData
	} else {
		stateData = map[string]interface{}{
			"input": inputData,
		}
	}

	rec, err := we.db.getNamespaceWorkflow(name, namespace)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, NewUncatchableError("direktiv.subflow.notExist", "workflow '%s' does not exist", name)
		}
		return nil, NewInternalError(err)
	}

	wf := new(model.Workflow)
	err = wf.Load(rec.Workflow)
	if err != nil {
		return nil, NewInternalError(err)
	}

	wli := new(workflowLogicInstance)
	wli.namespace = namespace
	wli.engine = we
	wli.wf = wf
	wli.data = stateData

	wli.id = fmt.Sprintf("%s/%s/%s", namespace, name, randSeq(6))
	wli.startData, err = json.MarshalIndent(wli.data, "", "  ")
	if err != nil {
		return nil, NewInternalError(err)
	}

	wli.logger, err = (*we.instanceLogger).LoggerFunc(namespace, wli.id)
	if err != nil {
		return nil, NewInternalError(err)
	}

	return wli, nil

}

func (we *workflowEngine) loadWorkflowLogicInstance(id string, step int) (context.Context, *workflowLogicInstance, error) {

	wli := new(workflowLogicInstance)
	wli.id = id
	wli.engine = we

	var success bool

	defer func() {
		if !success {
			wli.unlock()
		}
	}()

	ctx, err := wli.lock(time.Second * 5)
	if err != nil {
		return ctx, nil, NewInternalError(fmt.Errorf("cannot assume control of workflow instance lock: %v", err))
	}

	rec, err := we.db.getWorkflowInstance(context.Background(), id)
	if err != nil {
		return nil, nil, NewInternalError(err)
	}
	wli.rec = rec

	qwf, err := rec.QueryWorkflow().Only(ctx)
	if err != nil {
		wli.unlock()
		return ctx, nil, NewInternalError(fmt.Errorf("cannot resolve instance workflow: %v", err))
	}

	qns, err := qwf.QueryNamespace().Only(ctx)
	if err != nil {
		wli.unlock()
		return ctx, nil, NewInternalError(fmt.Errorf("cannot resolve instance namespace: %v", err))
	}

	wli.namespace = qns.ID

	wli.logger, err = (*we.instanceLogger).LoggerFunc(qns.ID, wli.id)
	if err != nil {
		wli.unlock()
		return ctx, nil, NewInternalError(fmt.Errorf("cannot initialize instance logger: %v", err))
	}

	err = json.Unmarshal([]byte(rec.StateData), &wli.data)
	if err != nil {
		wli.unlock()
		return ctx, nil, NewInternalError(fmt.Errorf("cannot load saved workflow state data: %v", err))
	}

	wli.wf = new(model.Workflow)
	wfrec, err := rec.QueryWorkflow().Only(ctx)
	if err != nil {
		wli.unlock()
		return ctx, nil, NewInternalError(fmt.Errorf("cannot load saved workflow from database: %v", err))
	}

	err = wli.wf.Load(wfrec.Workflow)
	if err != nil {
		wli.unlock()
		return ctx, nil, NewInternalError(fmt.Errorf("cannot load saved workflow definition: %v", err))
	}

	if rec.Status != "pending" && rec.Status != "running" {
		wli.unlock()
		return ctx, nil, NewInternalError(fmt.Errorf("aborting workflow logic: database records instance terminated"))
	}

	wli.step = step
	if len(rec.Flow) != wli.step {
		wli.unlock()
		return ctx, nil, NewInternalError(fmt.Errorf("aborting workflow logic: steps out of sync (expect/actual - %d/%d)", step, len(rec.Flow)))
	}

	state := rec.Flow[step-1]
	states := wli.wf.GetStatesMap()
	stateObject, exists := states[state]
	if !exists {
		wli.unlock()
		return ctx, nil, NewInternalError(fmt.Errorf("workflow cannot resolve state: %s", state))
	}

	init, exists := wli.engine.stateLogics[stateObject.GetType()]
	if !exists {
		wli.unlock()
		return ctx, nil, NewInternalError(fmt.Errorf("engine cannot resolve state type: %s", stateObject.GetType().String()))
	}

	stateLogic, err := init(wli.wf, stateObject)
	if err != nil {
		wli.unlock()
		return ctx, nil, NewInternalError(fmt.Errorf("cannot initialize state logic: %v", err))
	}
	wli.logic = stateLogic

	success = true

	return ctx, wli, nil

}

func (wli *workflowLogicInstance) lock(timeout time.Duration) (context.Context, error) {

	hash, err := hashstructure.Hash(wli.id, hashstructure.FormatV2, nil)
	if err != nil {
		return nil, NewInternalError(err)
	}

	wait := int(timeout.Seconds())

	conn, err := wli.engine.db.lockDB(hash, wait)
	if err != nil {
		return nil, NewInternalError(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	wli.engine.cancelsLock.Lock()
	wli.lockConn = conn
	wli.engine.cancels[wli.id] = cancel
	wli.engine.cancelsLock.Unlock()

	return ctx, nil

}

func (wli *workflowLogicInstance) unlock() {

	if wli.lockConn == nil {
		return
	}

	hash, err := hashstructure.Hash(wli.id, hashstructure.FormatV2, nil)
	if err != nil {
		log.Error(NewInternalError(err))
		return
	}

	wli.engine.cancelsLock.Lock()
	cancel := wli.engine.cancels[wli.id]
	delete(wli.engine.cancels, wli.id)
	cancel()

	err = wli.engine.db.unlockDB(hash, wli.lockConn)
	wli.lockConn = nil
	wli.engine.cancelsLock.Unlock()

	if err != nil {
		log.Error(NewInternalError(fmt.Errorf("Failed to unlock database mutex: %v", err)))
		return
	}

	return

}

func jq(input interface{}, command string) ([]interface{}, error) {

	data, err := json.Marshal(input)
	if err != nil {
		return nil, NewInternalError(err)
	}

	var x interface{}

	err = json.Unmarshal(data, &x)
	if err != nil {
		return nil, NewInternalError(err)
	}

	query, err := gojq.Parse(command)
	if err != nil {
		return nil, NewCatchableError(ErrCodeJQBadQuery, err.Error())
	}

	var output []interface{}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	iter := query.RunWithContext(ctx, x)

	for i := 0; ; i++ {

		v, ok := iter.Next()
		if !ok {
			break
		}

		if err, ok := v.(error); ok {
			return nil, NewUncatchableError("direktiv.jq.badCommand", err.Error())
		}

		output = append(output, v)

	}

	return output, nil

}

func jqOne(input interface{}, command string) (interface{}, error) {

	output, err := jq(input, command)
	if err != nil {
		return nil, err
	}

	if len(output) != 1 {
		return nil, NewCatchableError(ErrCodeJQNotObject, "the `jq` command produced multiple outputs")
	}

	return output, nil

}

func jqMustBeObject(input interface{}, command string) (map[string]interface{}, error) {

	x, err := jqOne(input, command)
	if err != nil {
		return nil, err
	}

	m, ok := x.(map[string]interface{})
	if !ok {
		return nil, NewCatchableError(ErrCodeJQNotObject, "the `jq` command produced a non-object output")
	}

	return m, nil

}

func (wli *workflowLogicInstance) JQ(command string) ([]interface{}, error) {

	return jq(wli.data, command)

}

func (wli *workflowLogicInstance) JQOne(command string) (interface{}, error) {

	return jqOne(wli.data, command)

}

func (wli *workflowLogicInstance) JQObject(command string) (map[string]interface{}, error) {

	return jqMustBeObject(wli.data, command)

}

func (wli *workflowLogicInstance) Log(msg string, a ...interface{}) {
	wli.logger.Info(fmt.Sprintf(msg, a...))
}

func (wli *workflowLogicInstance) Save(ctx context.Context, data []byte) error {
	var err error

	str := base64.StdEncoding.EncodeToString(data)

	wli.rec, err = wli.rec.Update().SetMemory(str).Save(ctx)
	if err != nil {
		return NewInternalError(err)
	}
	return nil
}

func (wli *workflowLogicInstance) StoreData(key string, val interface{}) error {

	m, ok := wli.data.(map[string]interface{})
	if !ok {
		return NewInternalError(errors.New("unable to store data because state data isn't a valid JSON object"))
	}

	m[key] = val

	return nil

}

func (wli *workflowLogicInstance) Transform(transform string) error {

	x, err := wli.JQObject(transform)
	if err != nil {
		return WrapCatchableError("unable to apply transform: %v", err)
	}

	wli.data = x
	return nil

}

func (wli *workflowLogicInstance) Retry(ctx context.Context, delayString string, multiplier float64) error {

	var err error
	var x interface{}

	err = json.Unmarshal([]byte(wli.rec.StateData), &x)
	if err != nil {
		return NewInternalError(err)
	}

	wli.data = x

	nextState := wli.rec.Flow[len(wli.rec.Flow)-1]

	attempt := wli.rec.Attempts + 1
	if multiplier == 0 {
		multiplier = 1.0
	}

	delay, err := duration.ParseISO8601(delayString)
	if err != nil {
		return NewInternalError(err)
	}

	multiplier = math.Pow(multiplier, float64(attempt))

	now := time.Now()
	t := delay.Shift(now)
	duration := t.Sub(now)
	duration = time.Duration(float64(duration) * multiplier)

	schedule := now.Add(duration)
	deadline := schedule.Add(time.Second * 5)
	duration = wli.logic.Deadline().Sub(now)
	deadline = deadline.Add(duration)

	var rec *ent.WorkflowInstance
	rec, err = wli.rec.Update().SetDeadline(deadline).Save(ctx)
	if err != nil {
		return err
	}
	wli.rec = rec
	wli.ScheduleSoftTimeout(deadline)

	if duration < time.Second*5 {
		time.Sleep(duration)
		wli.Log("Retrying failed workflow state.")
		go wli.Transition(nextState, attempt)
	} else {
		wli.Log("Scheduling a retry for the failed workflow state at approximate time: %s.", schedule.UTC().String())
		err = wli.engine.scheduleRetry(wli.id, nextState, wli.step, schedule)
		if err != nil {
			return err
		}
	}

	return nil

}

const timeoutFunction = "timeoutFunction"

type timeoutArgs struct {
	InstanceId string
	Step       int
	Soft       bool
}

func (we *workflowEngine) timeoutHandler(input []byte) error {

	args := new(timeoutArgs)
	err := json.Unmarshal(input, args)
	if err != nil {
		return err
	}

	if args.Soft {
		we.softCancelInstance(args.InstanceId, args.Step, "direktiv.cancels.timeout", "operation timed out")
	} else {
		we.hardCancelInstance(args.InstanceId, "direktiv.cancels.timeout", "workflow timed out")
	}

	return nil

}

func (wli *workflowLogicInstance) ScheduleHardTimeout(t time.Time) {

	var err error
	deadline := t
	oldId := fmt.Sprintf("timeout:%s:%d", wli.id, wli.step-1)
	id := fmt.Sprintf("timeout:%s:%d", wli.id, wli.step)

	if wli.step == 0 {
		id = fmt.Sprintf("timeout:%s", wli.id)
	}

	// cancel existing timeouts

	wli.engine.timer.actionTimerByName(oldId, deleteTimerAction)
	wli.engine.timer.actionTimerByName(id, deleteTimerAction)

	// schedule timeout

	args := &timeoutArgs{
		InstanceId: wli.id,
		Step:       wli.step,
		Soft:       false,
	}

	data, err := json.Marshal(args)
	if err != nil {
		log.Error(err)
	}

	_, err = wli.engine.timer.addOneShot(id, timeoutFunction, deadline, data)
	if err != nil {
		log.Error(err)
	}

}

func (wli *workflowLogicInstance) ScheduleSoftTimeout(t time.Time) {

	var err error
	deadline := t
	oldId := fmt.Sprintf("timeout:%s:%d", wli.id, wli.step-1)
	id := fmt.Sprintf("timeout:%s:%d", wli.id, wli.step)

	if wli.step == 0 {
		id = fmt.Sprintf("timeout:%s", wli.id)
	}

	// cancel existing timeouts

	wli.engine.timer.actionTimerByName(oldId, deleteTimerAction)
	wli.engine.timer.actionTimerByName(id, deleteTimerAction)

	// schedule timeout

	args := &timeoutArgs{
		InstanceId: wli.id,
		Step:       wli.step,
		Soft:       true,
	}

	data, err := json.Marshal(args)
	if err != nil {
		log.Error(err)
	}

	_, err = wli.engine.timer.addOneShot(id, timeoutFunction, deadline, data)
	if err != nil {
		log.Error(err)
	}

}

func (wli *workflowLogicInstance) Transition(nextState string, attempt int) {

	// NOTE: just about every error in this function represents something that is either
	// 	a serious and difficult-to-recover-from problem or something that should have
	// 	been validated long before we got here.

	ctx, err := wli.lock(time.Second * 5)
	if err != nil {
		log.Error(err)
		return
	}

	defer wli.unlock()

	if wli.step == 0 {
		t := time.Now()
		tSoft := time.Now().Add(time.Minute * 15)
		tHard := time.Now().Add(time.Minute * 20)
		if wli.wf.Timeouts != nil {
			s := wli.wf.Timeouts.Interrupt
			if s != "" {
				d, err := duration.ParseISO8601(s)
				if err != nil {
					log.Error(err)
					return
				}
				tSoft = d.Shift(t)
				tHard = tSoft.Add(time.Minute * 5)
			}
			s = wli.wf.Timeouts.Kill
			if s != "" {
				d, err := duration.ParseISO8601(s)
				if err != nil {
					log.Error(err)
					return
				}
				tHard = d.Shift(t)
			}
		}
		wli.ScheduleSoftTimeout(tSoft)
		wli.ScheduleHardTimeout(tHard)
	}

	if len(wli.rec.Flow) != wli.step {
		err = errors.New("workflow logic instance aborted for being tardy")
		log.Error(err)
		return
	}

	data, err := json.Marshal(wli.data)
	if err != nil {
		err = fmt.Errorf("engine cannot marshal state data for storage: %v", err)
		log.Error(err)
		return
	}

	if nextState == "" {
		panic("don't call this function with an empty nextState")
	}

	states := wli.wf.GetStatesMap()
	state, exists := states[nextState]
	if !exists {
		err = fmt.Errorf("workflow cannot resolve transition: %s", nextState)
		log.Error(err)
		return
	}

	init, exists := wli.engine.stateLogics[state.GetType()]
	if !exists {
		err = fmt.Errorf("engine cannot resolve state type: %s", state.GetType().String())
		log.Error(err)
		return
	}

	stateLogic, err := init(wli.wf, state)
	if err != nil {
		err = fmt.Errorf("cannot initialize state logic: %v", err)
		log.Error(err)
		return
	}
	wli.logic = stateLogic

	flow := append(wli.rec.Flow, nextState)
	wli.step++
	deadline := stateLogic.Deadline()

	var rec *ent.WorkflowInstance
	rec, err = wli.rec.Update().
		SetDeadline(deadline).
		SetNillableMemory(nil).
		SetAttempts(attempt).
		SetFlow(flow).
		SetStateData(string(data)).
		Save(ctx)
	if err != nil {
		log.Error(err)
		return
	}
	wli.rec = rec
	wli.ScheduleSoftTimeout(deadline)

	go func(we *workflowEngine, id, state string, step int) {
		ctx, wli, err := we.loadWorkflowLogicInstance(wli.id, wli.step)
		if err != nil {
			log.Errorf("cannot load workflow logic instance: %v", err)
			return
		}
		go wli.engine.runState(ctx, wli, nil, nil)
	}(wli.engine, wli.id, nextState, wli.step)

	return

}

func (we *workflowEngine) listenForEvents(ctx context.Context, wli *workflowLogicInstance, events []*model.ConsumeEventDefinition, all bool) error {

	wfid, err := wli.rec.QueryWorkflow().OnlyID(ctx)
	if err != nil {
		return err
	}

	signature, err := json.Marshal(&eventsWaiterSignature{
		InstanceID: wli.id,
		Step:       wli.step,
	})
	if err != nil {
		return err
	}

	var transformedEvents []*model.ConsumeEventDefinition

	for i := range events {

		ev := new(model.ConsumeEventDefinition)
		copier.Copy(ev, events[i])

		for k, v := range events[i].Context {

			str, ok := v.(string)
			if !ok {
				continue
			}

			if strings.HasPrefix(str, "{{") && strings.HasSuffix(str, "}}") {

				query := str[2 : len(str)-2]
				x, err := wli.JQOne(query)
				if err != nil {
					return fmt.Errorf("failed to execute jq query for key '%s' on event definition %d: %v", k, i, err)
				}

				switch x.(type) {
				case bool:
				case int:
				case string:
				case []byte:
				case time.Time:
				default:
					return fmt.Errorf("jq query on key '%s' for event definition %d returned an unacceptable type: %v", k, i, reflect.TypeOf(x))
				}

				ev.Context[k] = x

			}

		}

		transformedEvents = append(transformedEvents, ev)

	}

	_, err = we.db.addWorkflowEventListener(wfid, transformedEvents, signature, all)
	if err != nil {
		return err
	}

	wli.Log("Registered to receive events.")

	return nil

}
