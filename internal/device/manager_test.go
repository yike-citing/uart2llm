package device

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

type configSession struct {
	methods []string
	respond func(string, uint64) ([]byte, error)
}

func (s *configSession) Ready() bool { return true }
func (s *configSession) Call(_ context.Context, endpoint string, b []byte) ([]byte, error) {
	if endpoint != "rpc" {
		return nil, errors.New("unexpected endpoint")
	}
	var request struct {
		ID     uint64 `json:"id"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal(b, &request); err != nil {
		return nil, err
	}
	s.methods = append(s.methods, request.Method)
	return s.respond(request.Method, request.ID)
}

func TestStageApplyDistinguishesRejectedStageFromUnknownOutcome(t *testing.T) {
	for _, tc := range []struct {
		name       string
		failMethod string
		response   string
		callError  error
		notApplied bool
		rejected   bool
	}{
		{"stage validation rejection", "config.stage", `{"id":%d,"error":{"code":"device_error","message":"unknown configuration field"}}`, nil, true, true},
		{"stage transport timeout", "config.stage", "", context.DeadlineExceeded, false, false},
		{"stage truncated reply", "config.stage", `{"id":%d,"error":`, nil, false, false},
		{"stage mismatched reply ID", "config.stage", `{"id":999,"error":{"code":"device_error","message":"invalid"},"extra":%d}`, nil, false, false},
		{"apply explicit rejection", "config.apply", `{"id":%d,"error":{"code":"device_error","message":"NVS staging failed"}}`, nil, false, true},
		{"apply transport timeout", "config.apply", "", context.DeadlineExceeded, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &configSession{respond: func(method string, id uint64) ([]byte, error) {
				if method == "config.get" {
					return []byte(fmt.Sprintf(`{"id":%d,"result":{"pending":false}}`, id)), nil
				}
				if method == tc.failMethod {
					if tc.callError != nil {
						return nil, tc.callError
					}
					return []byte(fmt.Sprintf(tc.response, id)), nil
				}
				return []byte(fmt.Sprintf(`{"id":%d,"result":{}}`, id)), nil
			}}
			m := &Manager{s: s}
			_, err := m.StageApply(context.Background(), map[string]any{"wifi.ssid": "example"})
			if err == nil || errors.Is(err, ErrConfigNotApplied) != tc.notApplied {
				t.Fatalf("wrong outcome classification: %v", err)
			}
			var rejection *RPCRejection
			if errors.As(err, &rejection) != tc.rejected {
				t.Fatalf("wrong rejection type: %v", err)
			}
			if rejection != nil && rejection.Method != tc.failMethod {
				t.Fatalf("wrong rejection method: %s", rejection.Method)
			}
			want := []string{"config.stage"}
			if tc.failMethod == "config.apply" {
				want = append(want, "config.apply")
			} else if tc.rejected {
				want = append(want, "config.get")
			}
			if !reflect.DeepEqual(s.methods, want) {
				t.Fatalf("unexpected device commands: %v", s.methods)
			}
			if m.recovery != nil {
				t.Fatal("failed operation scheduled UART recovery")
			}
		})
	}
}

func TestRejectedStageKeepsSessionUsableForImmediateRetry(t *testing.T) {
	reject := true
	s := &configSession{respond: func(method string, id uint64) ([]byte, error) {
		if method == "config.get" {
			return []byte(fmt.Sprintf(`{"id":%d,"result":{"pending":false}}`, id)), nil
		}
		if reject {
			reject = false
			return []byte(fmt.Sprintf(`{"id":%d,"error":{"code":"device_error","message":"invalid value"}}`, id)), nil
		}
		return []byte(fmt.Sprintf(`{"id":%d,"result":{"pending":true}}`, id)), nil
	}}
	m := &Manager{s: s}
	defer m.Disconnect()
	if _, err := m.StageApply(context.Background(), map[string]any{"invalid": true}); !errors.Is(err, ErrConfigNotApplied) {
		t.Fatal(err)
	}
	if _, err := m.StageApply(context.Background(), map[string]any{"wifi.ssid": "valid"}); err != nil {
		t.Fatal("retry after known rejection failed:", err)
	}
	if !reflect.DeepEqual(s.methods, []string{"config.stage", "config.get", "config.stage", "config.apply"}) || m.recovery == nil {
		t.Fatalf("retry did not reach apply with recovery: %v", s.methods)
	}
}

func TestRejectedStageRequiresExplicitNoPendingTransactionSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name       string
		result     string
		queryError error
		notApplied bool
	}{
		{"no pending transaction", `{"pending":false}`, nil, true},
		{"previous host left applied transaction", `{"pending":true}`, nil, false},
		{"null pending", `{"pending":null}`, nil, false},
		{"absent pending", `{}`, nil, false},
		{"wrong pending type", `{"pending":"false"}`, nil, false},
		{"null snapshot", `null`, nil, false},
		{"query timed out", "", context.DeadlineExceeded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &configSession{respond: func(method string, id uint64) ([]byte, error) {
				switch method {
				case "config.stage":
					return []byte(fmt.Sprintf(`{"id":%d,"error":{"code":"device_error","message":"stage rejected"}}`, id)), nil
				case "config.get":
					if tc.queryError != nil {
						return nil, tc.queryError
					}
					return []byte(fmt.Sprintf(`{"id":%d,"result":%s}`, id, tc.result)), nil
				default:
					t.Errorf("unexpected command %s", method)
					return nil, errors.New("unexpected command")
				}
			}}
			m := &Manager{s: s}
			_, err := m.StageApply(context.Background(), map[string]any{"wifi.ssid": "example"})
			if err == nil || errors.Is(err, ErrConfigNotApplied) != tc.notApplied {
				t.Fatalf("wrong safe-resume classification: %v", err)
			}
			if !reflect.DeepEqual(s.methods, []string{"config.stage", "config.get"}) {
				t.Fatalf("did not verify snapshot under transaction: %v", s.methods)
			}
		})
	}
}
