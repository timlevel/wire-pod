package scheduler

import (
	"context"
	"errors"
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
	if s.wait {
		<-s.ctx.Done()
		return nil, s.ctx.Err()
	}
	return &vectorpb.BehaviorControlResponse{ResponseType: &vectorpb.BehaviorControlResponse_ControlGrantedResponse{ControlGrantedResponse: &vectorpb.ControlGrantedResponse{}}}, nil
}

type fakeRobot struct {
	vectorpb.ExternalInterfaceClient
	stream     *fakeStream
	calls      []string
	speech     string
	failSpeech bool
	failDrive  bool
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
func (f *fakeRobot) DriveOnCharger(context.Context, *vectorpb.DriveOnChargerRequest, ...grpc.CallOption) (*vectorpb.DriveOnChargerResponse, error) {
	f.calls = append(f.calls, "on")
	return &vectorpb.DriveOnChargerResponse{Status: okStatus(), Result: vectorpb.BehaviorResults_BEHAVIOR_COMPLETE_STATE}, nil
}
func (f *fakeRobot) SayText(_ context.Context, r *vectorpb.SayTextRequest, _ ...grpc.CallOption) (*vectorpb.SayTextResponse, error) {
	f.calls = append(f.calls, "say")
	f.speech = r.Text
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
				want = "off"
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
