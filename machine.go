package statemachine

import (
	"context"
	"fmt"
	"reflect"
	"sync/atomic"

	"github.com/filecoin-project/go-statestore"
	xerrors "golang.org/x/xerrors"

	logging "github.com/ipfs/go-log/v2"
)

var log = logging.Logger("evtsm")

var ErrTerminated = xerrors.New("normal shutdown of state machine")

type Event struct {
	User interface{}
}

// Planner processes in queue events
// It returns:
// 1. a handler of type -- func(ctx Context, st <T>) (func(*<T>), error), where <T> is the typeOf(User) param
// 2. the number of events processed
// 3. an error if occured
type Planner func(events []Event, user interface{}) (interface{}, uint64, error)

type StateMachine struct {
	planner  Planner
	eventsIn chan Event

	name      interface{}
	st        *statestore.StoredState
	stateType reflect.Type

	stageDone chan struct{}
	closing   chan struct{}
	closed    chan struct{}

	busy int32
}

func (fsm *StateMachine) run() {
	defer close(fsm.closed)

	var pendingEvents []Event

	for {
		// NOTE: This requires at least one event to be sent to trigger a stage
		//  This means that after restarting the state machine users of this
		//  code must send a 'restart' event
		//log.Infof("fsmbusy%v", fsm.busy)
		var aaa Event
		select {
		case evt := <-fsm.eventsIn:
			//log.Infof("evt := <-fsm.eventsIn")
			pendingEvents = append(pendingEvents, evt)
			//log.Infof("evt := <-fsm.eventsIn%+v", pendingEvents)
			aaa = evt
			log.Debugw("appended new pending evt", "len(pending)", len(pendingEvents))
		case <-fsm.stageDone:
			if len(pendingEvents) == 0 {
				continue
			}
		case <-fsm.closing:
			return
		}
		//log.Infof("fsm.busy%v", fsm.busy)
		//log.Infof("fsmBeee,eventsIn%v,name,%v,stateType%v", fsm.eventsIn, fsm.name, fsm.stateType)

		bbb := fmt.Sprintf("%v", aaa)
		log.Debugf("bbb%v", bbb)
		if bbb == "{{Removed}}" || bbb == "{{BeeRemoving}}" {
			//log.Infof("bbb%v", bbb)
			atomic.StoreInt32(&fsm.busy, 0)
		}
		if atomic.CompareAndSwapInt32(&fsm.busy, 0, 1) {
			log.Debugw("sm run in critical zone", "len(pending)", len(pendingEvents))
			//log.Infof("CompareAndSwapInt32%+v", fsm.busy)
			var nextStep interface{}
			var ustate interface{}
			var processed uint64
			var terminated bool

			err := fsm.mutateUser(func(user interface{}) (err error) {
				//log.Infof("fsm.mutateUser")
				nextStep, processed, err = fsm.planner(pendingEvents, user)
				ustate = user
				//log.Infof("fsm.mutateUser %v", user)
				if xerrors.Is(err, ErrTerminated) {
					terminated = true
					return nil
				}
				return err
			})
			//log.Infof("terminated %v", terminated)
			if terminated {
				return
			}
			if err != nil {
				log.Errorf("Executing event planner failed: %+v", err)
				return
			}
			//log.Infof("processed%v,pendingEvents%v", processed, pendingEvents)
			if processed < uint64(len(pendingEvents)) {
				//log.Infof("xiaoyu")
				pendingEvents = pendingEvents[processed:]
			} else {
				pendingEvents = nil
			}

			ctx := Context{
				ctx: context.TODO(),
				send: func(evt interface{}) error {
					return fsm.send(Event{User: evt})
				},
			}

			go func() {
				defer log.Debugw("leaving critical zone and resetting atomic var to zero", "len(pending)", len(pendingEvents))

				if nextStep != nil {
					res := reflect.ValueOf(nextStep).Call([]reflect.Value{reflect.ValueOf(ctx), reflect.ValueOf(ustate).Elem()})

					if res[0].Interface() != nil {
						log.Errorf("executing step: %+v", res[0].Interface().(error)) // TODO: propagate top level
						return
					}
				}

				//log.Infof("atomic.StoreInt32")
				atomic.StoreInt32(&fsm.busy, 0)
				//log.Infof("atomic.StoreInt32%v", fsm.busy)
				fsm.stageDone <- struct{}{}
			}()

		}
	}
}

func (fsm *StateMachine) mutateUser(cb func(user interface{}) error) error {
	mutt := reflect.FuncOf([]reflect.Type{reflect.PtrTo(fsm.stateType)}, []reflect.Type{reflect.TypeOf(new(error)).Elem()}, false)

	mutf := reflect.MakeFunc(mutt, func(args []reflect.Value) (results []reflect.Value) {
		err := cb(args[0].Interface())
		return []reflect.Value{reflect.ValueOf(&err).Elem()}
	})

	return fsm.st.Mutate(mutf.Interface())
}

func (fsm *StateMachine) send(evt Event) error {
	select {
	case <-fsm.closed:
		return ErrTerminated
	case fsm.eventsIn <- evt: // TODO: ctx, at least
		//log.Infof("in channel")
		return nil
	}
}

func (fsm *StateMachine) stop(ctx context.Context) error {
	close(fsm.closing)

	select {
	case <-fsm.closed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
