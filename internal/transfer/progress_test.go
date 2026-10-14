package transfer

import (
	"testing"
	"time"
)

func TestProgressRetryAndSnapshotOwnership(t *testing.T) {
	st := &state{started: time.Now()}
	st.beginFile(job{remote: "/one", size: 100})
	st.beginFile(job{remote: "/two", size: 100})
	st.recordBytes("/one", 40, 40)
	st.recordBytes("/two", 30, 20) // Local short write: wire bytes still count.
	before := st.sample()
	if before.Bytes != 70 || before.ProgressBytes != 60 || before.Active != 2 || len(before.ActiveFiles) != 2 || before.ActiveFiles[0].Bytes != 40 || before.ActiveFiles[1].Bytes != 20 {
		t.Fatalf("incorrect initial progress: %+v", before)
	}
	st.resetFile("/two")
	retry := st.sample()
	if retry.Bytes != 70 || retry.ProgressBytes != 40 || retry.ActiveFiles[1].Bytes != 0 {
		t.Fatalf("failed attempt not rolled back: %+v", retry)
	}
	st.recordBytes("/two", 100, 100)
	st.finishFile("/two", true)
	after := st.sample()
	if after.Bytes != 170 || after.ProgressBytes != 140 || after.Active != 1 || len(after.ActiveFiles) != 1 || after.ActiveFiles[0].Path != "/one" {
		t.Fatalf("incorrect retry/completion progress: %+v", after)
	}
	if before.ActiveFiles[1].Bytes != 20 || retry.ActiveFiles[1].Bytes != 0 {
		t.Fatal("published snapshots changed during retry")
	}
}
