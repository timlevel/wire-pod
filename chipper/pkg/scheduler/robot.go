package scheduler

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fforchino/vector-go-sdk/pkg/vectorpb"
	"github.com/kercre123/wire-pod/chipper/pkg/vars"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type tokenAuth string

func (t tokenAuth) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(t)}, nil
}
func (t tokenAuth) RequireTransportSecurity() bool { return true }

func readRobots(path string) (vars.RobotInfoStore, error) {
	var info vars.RobotInfoStore
	b, err := os.ReadFile(path)
	if err != nil {
		return info, err
	}
	err = json.Unmarshal(b, &info)
	return info, err
}

// Read credentials on each run so reauthenticating a robot needs no restart.
func RobotRunner(path string) Runner {
	return func(ctx context.Context, s Schedule, when time.Time) error {
		info, err := readRobots(path)
		if err != nil {
			return err
		}
		for _, bot := range info.Robots {
			if !strings.EqualFold(bot.Esn, s.ESN) {
				continue
			}
			token := bot.GUID
			if token == "" {
				token = info.GlobalGUID
			}
			if token == "" || bot.IPAddress == "" {
				return errors.New("robot SDK credentials or address missing")
			}
			dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			// Vector uses a device-issued certificate, like the existing SDK connection.
			conn, err := grpc.DialContext(dialCtx, net.JoinHostPort(bot.IPAddress, "443"), grpc.WithBlock(),
				grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})),
				grpc.WithPerRPCCredentials(tokenAuth(token)))
			if err != nil {
				return fmt.Errorf("connect to robot: %w", err)
			}
			defer conn.Close()
			return execute(ctx, vectorpb.NewExternalInterfaceClient(conn), s, when)
		}
		return errors.New("robot is not in SDK configuration; authenticate it in Bot Settings")
	}
}

// RobotList exposes serials only, never SDK credentials.
func RobotList(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET required", 405)
			return
		}
		info, err := readRobots(path)
		if err != nil && !os.IsNotExist(err) {
			http.Error(w, "could not read robot list", 500)
			return
		}
		serials := make([]string, 0, len(info.Robots))
		for _, bot := range info.Robots {
			serials = append(serials, bot.Esn)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(serials)
	}
}

func expandText(text string, when time.Time, zone string) string {
	loc, _ := time.LoadLocation(zone)
	local := when.In(loc)
	hour := local.Hour() % 12
	if hour == 0 {
		hour = 12
	}
	return strings.NewReplacer("{hour}", strconv.Itoa(hour), "{hour24}", strconv.Itoa(local.Hour()), "{time}", local.Format("3:04 PM")).Replace(text)
}

func execute(ctx context.Context, client vectorpb.ExternalInterfaceClient, s Schedule, when time.Time) (runErr error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := client.BehaviorControl(ctx)
	if err != nil {
		return err
	}
	defer stream.CloseSend()
	err = stream.Send(&vectorpb.BehaviorControlRequest{RequestType: &vectorpb.BehaviorControlRequest_ControlRequest{ControlRequest: &vectorpb.ControlRequest{Priority: vectorpb.ControlRequest_DEFAULT}}})
	if err != nil {
		return err
	}
	for {
		response, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("wait for behavior control: %w", err)
		}
		if response.GetControlGrantedResponse() != nil {
			break
		}
	}
	defer stream.Send(&vectorpb.BehaviorControlRequest{RequestType: &vectorpb.BehaviorControlRequest_ControlRelease{ControlRelease: &vectorpb.ControlRelease{}}})
	// If an intermediate action fails after undocking, still try the requested return.
	undocked, returned := false, false
	wantsReturn := s.Actions[len(s.Actions)-1].Type == "drive_on_charger"
	defer func() {
		if runErr != nil && undocked && !returned && wantsReturn && ctx.Err() == nil {
			if err := perform(ctx, client, Action{Type: "drive_on_charger"}, when, s.Timezone); err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("return to charger: %w", err))
			}
		}
	}()
	for i, a := range s.Actions {
		if err := perform(ctx, client, a, when, s.Timezone); err != nil {
			return fmt.Errorf("action %d (%s): %w", i+1, a.Type, err)
		}
		if a.Type == "drive_off_charger" {
			undocked = true
		}
		if a.Type == "drive_on_charger" {
			returned = true
		}
	}
	return nil
}

func statusError(status *vectorpb.ResponseStatus) error {
	if status == nil {
		return errors.New("robot returned no status")
	}
	code := status.GetCode()
	if code != vectorpb.ResponseStatus_OK && code != vectorpb.ResponseStatus_RESPONSE_RECEIVED {
		return fmt.Errorf("robot status: %s", code)
	}
	return nil
}

func behaviorError(status *vectorpb.ResponseStatus, result vectorpb.BehaviorResults, err error) error {
	if err != nil {
		return err
	}
	if err = statusError(status); err != nil {
		return err
	}
	if result != vectorpb.BehaviorResults_BEHAVIOR_COMPLETE_STATE {
		return fmt.Errorf("robot behavior: %s", result)
	}
	return nil
}

func perform(ctx context.Context, c vectorpb.ExternalInterfaceClient, a Action, when time.Time, zone string) error {
	// Leave time in the overall run budget for a return-to-charger attempt.
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	switch a.Type {
	case "say_text":
		r, err := c.SayText(ctx, &vectorpb.SayTextRequest{Text: expandText(a.Text, when, zone), UseVectorVoice: true, DurationScalar: 1})
		if err != nil {
			return err
		}
		if err = statusError(r.GetStatus()); err != nil {
			return err
		}
		if r.GetState() != vectorpb.SayTextResponse_FINISHED {
			return fmt.Errorf("speech state: %s", r.GetState())
		}
		return nil
	case "play_animation":
		r, err := c.PlayAnimation(ctx, &vectorpb.PlayAnimationRequest{Animation: &vectorpb.Animation{Name: a.Animation}, Loops: 1})
		return behaviorError(r.GetStatus(), r.GetResult(), err)
	case "drive_off_charger":
		r, err := c.DriveOffCharger(ctx, &vectorpb.DriveOffChargerRequest{})
		return behaviorError(r.GetStatus(), r.GetResult(), err)
	case "drive_on_charger":
		r, err := c.DriveOnCharger(ctx, &vectorpb.DriveOnChargerRequest{})
		return behaviorError(r.GetStatus(), r.GetResult(), err)
	}
	return fmt.Errorf("unknown action %q", a.Type)
}
