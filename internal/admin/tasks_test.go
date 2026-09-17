package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

type tasksDevice struct {
	fakeDevice
	respond func(map[string]any) json.RawMessage
}

func (*tasksDevice) Status() map[string]any { return map[string]any{"connected": true, "paired": true} }
func (d *tasksDevice) RPC(_ context.Context, method string, params any) (json.RawMessage, error) {
	if method != "tasks.get" {
		return nil, fmt.Errorf("unexpected method %s", method)
	}
	return d.respond(params.(map[string]any)), nil
}

func TestTaskCursorPreservesSurvivorsWhenEarlierTasksDisappear(t *testing.T) {
	s := testAdmin(t)
	calls := 0
	d := &tasksDevice{respond: func(params map[string]any) json.RawMessage {
		calls++
		after, ok := params["after_id"].(uint32)
		if !ok {
			t.Fatal("task cursor not sent")
		}
		// Task 1 disappears between pages; offset 2 would now skip task 3.
		ids := []uint32{1, 2, 3, 4}
		if calls > 1 {
			ids = []uint32{2, 3, 4, 5}
		}
		var items []map[string]uint32
		for _, id := range ids {
			if id > after {
				items = append(items, map[string]uint32{"id": id})
			}
		}
		var next any
		if len(items) > 2 {
			items = items[:2]
			next = items[1]["id"]
		}
		b, _ := json.Marshal(map[string]any{"items": items, "next_after_id": next, "next_offset": 2})
		return b
	}}
	s.Device = d
	w := callAdmin(s, "GET", "/tasks", "", "admin-secret", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result struct {
		Device []struct {
			ID uint32 `json:"id"`
		} `json:"device"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var ids []uint32
	for _, task := range result.Device {
		ids = append(ids, task.ID)
	}
	if !reflect.DeepEqual(ids, []uint32{1, 2, 3, 4, 5}) || calls != 3 {
		t.Fatalf("lost surviving task or duplicated item: %v (%d pages)", ids, calls)
	}
}

func TestTaskPaginationFallsBackToLegacyOffsets(t *testing.T) {
	s := testAdmin(t)
	calls := 0
	s.Device = &tasksDevice{respond: func(params map[string]any) json.RawMessage {
		calls++
		if calls == 1 {
			return json.RawMessage(`{"items":[{"id":1}],"next_offset":1}`)
		}
		if _, ok := params["after_id"]; ok || params["offset"] != 1 {
			t.Fatal("did not fall back to offset", params)
		}
		return json.RawMessage(`{"items":[{"id":2}],"next_offset":null}`)
	}}
	w := callAdmin(s, "GET", "/tasks", "", "admin-secret", "")
	if w.Code != 200 || calls != 2 {
		t.Fatal(w.Code, calls, w.Body.String())
	}
}

func TestTaskCursorRejectsNonProgressAndDowngrade(t *testing.T) {
	for _, reply := range []string{
		`{"items":[],"next_after_id":2}`, // Repeats the previous cursor.
		`{"items":[],"next_after_id":1}`,
		`{"items":[],"next_after_id":-1}`,
		`{"items":[],"next_after_id":1.5}`,
		`{"items":[],"next_after_id":4294967296}`,
		`{"items":[],"next_offset":3}`, // Cursor-aware peer may not switch schemes mid-enumeration.
	} {
		t.Run(reply, func(t *testing.T) {
			s := testAdmin(t)
			calls := 0
			s.Device = &tasksDevice{respond: func(map[string]any) json.RawMessage {
				calls++
				if calls == 1 {
					return json.RawMessage(`{"items":[{"id":2}],"next_after_id":2}`)
				}
				return json.RawMessage(reply)
			}}
			w := callAdmin(s, "GET", "/tasks", "", "admin-secret", "")
			if w.Code != 400 || calls != 2 {
				t.Fatal(w.Code, calls, w.Body.String())
			}
		})
	}
}
