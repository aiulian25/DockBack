package api

import (
	"encoding/json"
	"testing"
	"time"

	"dockback/internal/store"
)

// The Backups page used to poll the drill list every five seconds — twelve
// requests a minute, for the whole time a tab was open, to catch a verdict that
// arrives once. It can only stop doing that if a finished drill announces
// itself, because a drill runs for minutes: nothing else would tell the
// operator the result without that poll.
func TestAFinishedDrillAnnouncesItself(t *testing.T) {
	s := &Server{store: testStore(t), bcast: newBroadcaster(64)}
	const backupID, nodeID = "bk-1", "n1"

	ch, _ := s.bcast.subscribe()
	defer s.bcast.unsubscribe(ch)

	b := &store.Backup{ID: backupID, NodeID: nodeID, TargetName: "app", Status: "success", CreatedAt: time.Now().Unix()}
	if err := s.store.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	if err := s.recordDrillResult(b, true, "restored cleanly into a sandbox"); err != nil {
		t.Fatal(err)
	}

	select {
	case msg := <-ch:
		if msg.event != "backup.status" {
			t.Fatalf("event %q, want backup.status — the page listens for that one", msg.event)
		}
		var got backupStatusEvent
		if err := json.Unmarshal([]byte(msg.data), &got); err != nil {
			t.Fatal(err)
		}
		if got.BackupID != backupID || got.NodeID != nodeID {
			t.Errorf("event names %s/%s, want %s/%s — the page reloads the wrong row", got.NodeID, got.BackupID, nodeID, backupID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event; the drill verdict would only appear on the next slow poll")
	}
}

// The verdict has to be readable from the store when that event lands, or the
// reload it triggers fetches the old row.
func TestDrillResultIsRecordedBeforeItIsAnnounced(t *testing.T) {
	st := testStore(t)
	const backupID = "bk-1"
	if err := st.CreateBackup(&store.Backup{ID: backupID, NodeID: "n1", TargetName: "app", Status: "success", CreatedAt: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDrill(backupID, false, "restore failed in the sandbox", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	drills, err := st.ListDrills()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range drills {
		if d.BackupID == backupID {
			found = true
			if d.OK {
				t.Error("a failed drill must be recorded as failed")
			}
			if d.Detail == "" {
				t.Error("the detail is what the row shows; it must survive")
			}
		}
	}
	if !found {
		t.Fatal("the drill result must be readable as soon as it is announced")
	}
}
