package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/fforchino/vector-go-sdk/pkg/vectorpb"
	"google.golang.org/grpc"
)

type fakeStream struct {
	grpc.ClientStream
	ctx      context.Context
	requests []*vectorpb.BehaviorControlRequest
	fail     bool
	wait     bool
	granted  bool
	events   chan *vectorpb.BehaviorControlResponse
}

func (s *fakeStream) Send(r *vectorpb.BehaviorControlRequest) error {
	s.requests = append(s.requests, r)
	return nil
}
func (s *fakeStream) CloseSend() error { return nil }
func (s *fakeStream) Recv() (*vectorpb.BehaviorControlResponse, error) {
	if s.fail {
		return nil, io.EOF
	}
	if s.wait || s.granted {
		if s.events != nil {
			select {
			case response := <-s.events:
				return response, nil
			case <-s.ctx.Done():
				return nil, s.ctx.Err()
			}
		}
		<-s.ctx.Done()
		return nil, s.ctx.Err()
	}
	s.granted = true
	return &vectorpb.BehaviorControlResponse{ResponseType: &vectorpb.BehaviorControlResponse_ControlGrantedResponse{ControlGrantedResponse: &vectorpb.ControlGrantedResponse{}}}, nil
}

type fakeRobot struct {
	vectorpb.ExternalInterfaceClient
	stream     *fakeStream
	calls      []string
	speech     string
	failSpeech bool
	failDrive  bool
	onSay      func(context.Context) error
	onDock     func(context.Context) error
}

func (f *fakeRobot) BehaviorControl(ctx context.Context, _ ...grpc.CallOption) (vectorpb.ExternalInterface_BehaviorControlClient, error) {
	f.stream.ctx = ctx
	return f.stream, nil
}
func okStatus() *vectorpb.ResponseStatus {
	return &vectorpb.ResponseStatus{Code: vectorpb.ResponseStatus_OK}
}
func (f *fakeRobot) DriveOffCharger(context.Context, *vectorpb.DriveOffChargerRequest, ...grpc.CallOption) (*vectorpb.DriveOffChargerResponse, error) {
	f.calls = append(f.calls, "off")
	result := vectorpb.BehaviorResults_BEHAVIOR_COMPLETE_STATE
	if f.failDrive {
		result = vectorpb.BehaviorResults_BEHAVIOR_WONT_ACTIVATE_STATE
	}
	return &vectorpb.DriveOffChargerResponse{Status: okStatus(), Result: result}, nil
}
func (f *fakeRobot) DriveOnCharger(ctx context.Context, _ *vectorpb.DriveOnChargerRequest, _ ...grpc.CallOption) (*vectorpb.DriveOnChargerResponse, error) {
	f.calls = append(f.calls, "on")
	if f.onDock != nil {
		if err := f.onDock(ctx); err != nil {
			return nil, err
		}
	}
	return &vectorpb.DriveOnChargerResponse{Status: okStatus(), Result: vectorpb.BehaviorResults_BEHAVIOR_COMPLETE_STATE}, nil
}
func (f *fakeRobot) SayText(ctx context.Context, r *vectorpb.SayTextRequest, _ ...grpc.CallOption) (*vectorpb.SayTextResponse, error) {
	f.calls = append(f.calls, "say")
	f.speech = r.Text
	if f.onSay != nil {
		if err := f.onSay(ctx); err != nil {
			return nil, err
		}
	}
	if f.failSpeech {
		return nil, errors.New("speech failed")
	}
	return &vectorpb.SayTextResponse{Status: okStatus(), State: vectorpb.SayTextResponse_FINISHED}, nil
}
func TestHourlyRobotSequence(t *testing.T) {
	for _, failure := range []string{"", "speech", "drive"} {
		t.Run(failure, func(t *testing.T) {
			f := &fakeRobot{stream: &fakeStream{}, failSpeech: failure == "speech", failDrive: failure == "drive"}
			err := execute(context.Background(), f, hourly(), time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC))
			if (err != nil) != (failure != "") {
				t.Fatal(err)
			}
			want := "off,say,on"
			if failure == "drive" {
				want = "off,on"
			}
			if strings.Join(f.calls, ",") != want {
				t.Fatal(f.calls)
			}
			if failure != "drive" && f.speech != "It is 12 o'clock!" {
				t.Fatal(f.speech)
			}
			if len(f.stream.requests) != 2 || f.stream.requests[1].GetControlRelease() == nil {
				t.Fatal("behavior control not released")
			}
		})
	}
}
func TestControlFailureAndTimeout(t *testing.T) {
	for _, wait := range []bool{false, true} {
		f := &fakeRobot{stream: &fakeStream{fail: !wait, wait: wait}}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		err := execute(ctx, f, hourly(), time.Now())
		cancel()
		if err == nil || len(f.calls) > 0 {
			t.Fatalf("actions ran without behavior control: %v", err)
		}
	}
}
func TestBehaviorFailureIsNotSuccess(t *testing.T) {
	if err := behaviorError(okStatus(), vectorpb.BehaviorResults_BEHAVIOR_INVALID_STATE, nil); err == nil {
		t.Fatal("invalid state succeeded")
	}
	if err := statusError(&vectorpb.ResponseStatus{Code: vectorpb.ResponseStatus_FORBIDDEN}); err == nil {
		t.Fatal("forbidden succeeded")
	}
}

func TestRecoveryAfterRunCancellation(t *testing.T) {
	for _, finalDock := range []bool{true, false} {
		t.Run(fmt.Sprint(finalDock), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := &fakeRobot{stream: &fakeStream{}}
			f.onSay = func(ctx context.Context) error { cancel(); <-ctx.Done(); return ctx.Err() }
			f.onDock = func(ctx context.Context) error {
				if ctx.Err() != nil || f.stream.ctx.Err() != nil {
					t.Error("recovery inherited expired run/stream context")
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Error("recovery must be bounded")
				}
				return ctx.Err()
			}
			s := hourly()
			if !finalDock {
				s.Actions = s.Actions[:2]
			}
			err := execute(ctx, f, s, time.Now())
			if !errors.Is(err, context.Canceled) || strings.Join(f.calls, ",") != "off,say,on" {
				t.Fatalf("missing recovery: %v, %v", err, f.calls)
			}
		})
	}
}
func TestControlLossCancelsAction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	f := &fakeRobot{stream: &fakeStream{events: make(chan *vectorpb.BehaviorControlResponse, 1)}}
	f.onSay = func(ctx context.Context) error {
		f.stream.events <- &vectorpb.BehaviorControlResponse{ResponseType: &vectorpb.BehaviorControlResponse_ControlLostEvent{ControlLostEvent: &vectorpb.ControlLostResponse{}}}
		<-ctx.Done()
		return ctx.Err()
	}
	err := execute(ctx, f, hourly(), time.Now())
	if err == nil || !strings.Contains(err.Error(), "behavior control lost") {
		t.Fatalf("unclear loss error: %v", err)
	}
	if strings.Join(f.calls, ",") != "off,say" {
		t.Fatalf("action ran after control loss: %v", f.calls)
	}
}

func TestRecoveryAfterRunDeadlineAndRecoveryTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	f := &fakeRobot{stream: &fakeStream{}}
	f.onSay = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	f.onDock = func(ctx context.Context) error {
		if f.stream.ctx.Err() != nil {
			t.Error("control stream expired before recovery")
		}
		<-ctx.Done()
		return ctx.Err()
	}
	err := executeWithLimits(ctx, f, hourly(), time.Now(), time.Second, 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "return to charger:") || strings.Join(f.calls, ",") != "off,say,on" {
		t.Fatalf("deadline recovery: %v, %v", err, f.calls)
	}
	if f.stream.ctx.Err() == nil {
		t.Fatal("control stream leaked")
	}
}
func TestControlGrantWaitIsBounded(t *testing.T) {
	f := &fakeRobot{stream: &fakeStream{wait: true}}
	err := executeWithLimits(context.Background(), f, hourly(), time.Now(), 10*time.Millisecond, time.Second)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "wait for behavior control:") || len(f.calls) != 0 {
		t.Fatalf("grant wait: %v, %v", err, f.calls)
	}
	if f.stream.ctx.Err() == nil {
		t.Fatal("control stream leaked")
	}
}
func TestControlLossDuringRecovery(t *testing.T) {
	f := &fakeRobot{stream: &fakeStream{events: make(chan *vectorpb.BehaviorControlResponse, 1)}, failSpeech: true}
	f.onDock = func(ctx context.Context) error {
		f.stream.events <- &vectorpb.BehaviorControlResponse{ResponseType: &vectorpb.BehaviorControlResponse_ControlLostEvent{ControlLostEvent: &vectorpb.ControlLostResponse{}}}
		<-ctx.Done()
		return context.Cause(ctx)
	}
	err := executeWithLimits(context.Background(), f, hourly(), time.Now(), time.Second, time.Second)
	if err == nil || !strings.Contains(err.Error(), "return to charger:") || !strings.Contains(err.Error(), "behavior control lost") {
		t.Fatalf("recovery loss: %v", err)
	}
}
