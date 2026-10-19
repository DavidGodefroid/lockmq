//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A peek requeues: the queue keeps its depth, the messages come back.
func TestPeekLeavesDepth(t *testing.T) {
	each(t, func(t *testing.T, b *broker) {
		r := newRig(t, b, "peek")
		before, err := b.queueDepth(dlqName)
		if err != nil {
			t.Fatal(err)
		}
		r.io.answer("y")
		data := okData(t, r.run("peek", dlqName, 2, ""))
		msgs, _ := data["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("peeked %d messages", len(msgs))
		}
		after, err := b.waitDepth(dlqName, before)
		if err != nil || after != before {
			t.Errorf("depth %d → %d (%v)", before, after, err)
		}
		if r.io.promptCount() != 1 {
			t.Errorf("prompts %d", r.io.promptCount())
		}
		if raw := strings.Join(r.io.out, "\n"); !strings.Contains(raw, "NOT READ-ONLY") {
			t.Errorf("screen:\n%s", raw)
		}
	})
}

// On a quorum queue with delivery-limit 5 and headroom 3, peeks stop
// before the message could be dead-lettered.
func TestPeekRefusedNearLimit(t *testing.T) {
	each(t, func(t *testing.T, b *broker) {
		r := newRig(t, b, "peek")
		dlqBefore, err := b.queueDepth(dlqName)
		if err != nil {
			t.Fatal(err)
		}
		ok := 0
		for range 6 {
			r.io.answer("y")
			resp := r.run("peek", quorumName, 1, "")
			if code := refusedCode(resp); code != "" {
				if code != "E_DELIVERY_LIMIT" {
					t.Fatalf("refused with %s", code)
				}
				break
			}
			okData(t, resp)
			ok++
		}
		if ok == 0 || ok >= 6 {
			t.Fatalf("%d peeks before the refusal", ok)
		}
		depth, err := b.waitDepth(quorumName, queueCount)
		if err != nil || depth != queueCount {
			t.Errorf("quorum depth %d (%v), want %d", depth, err, queueCount)
		}
		if ok != 3 {
			t.Errorf("%d peeks before the refusal, want 3 (limit %d, headroom 3)", ok, quorumLimit)
		}
		dlqAfter, _ := b.waitDepth(dlqName, dlqBefore)
		if dlqAfter != dlqBefore {
			t.Errorf("dead-lettered: DLQ %d → %d", dlqBefore, dlqAfter)
		}
	})
}

// A profile without tier peek refuses a peek before any broker call.
func TestPeekOffByDefault(t *testing.T) {
	each(t, func(t *testing.T, b *broker) {
		r := newRig(t, b, "read")
		if code := refusedCode(r.run("peek", dlqName, 1, "")); code != "E_TIER" {
			t.Fatalf("peek on tier read: %q", code)
		}
		if r.io.promptCount() != 0 {
			t.Errorf("prompted")
		}
	})
}

// read → peek written in the file waits for the human's confirmation;
// peek → read applies at once.
func TestTierEditNeedsConfirmation(t *testing.T) {
	each(t, func(t *testing.T, b *broker) {
		r := newRig(t, b, "read")
		r.writeConfig("peek")
		r.srv.CheckPolicy()
		if code := refusedCode(r.run("peek", dlqName, 1, "")); code != "E_TIER" {
			t.Fatalf("unconfirmed peek: %q", code)
		}
		r.io.answer("y")
		r.srv.Command(t.Context(), ":review")
		r.io.answer("y")
		okData(t, r.run("peek", dlqName, 1, ""))

		r.writeConfig("read")
		// The poll compares file contents; make sure the edit is seen as new.
		now := time.Now().Add(time.Second)
		_ = os.Chtimes(filepath.Join(r.root, ".lockmq", "config.toml"), now, now)
		r.srv.CheckPolicy()
		if code := refusedCode(r.run("peek", dlqName, 1, "")); code != "E_TIER" {
			t.Fatalf("peek after tightening to read: %q", code)
		}
	})
}
