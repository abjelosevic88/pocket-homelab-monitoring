package jobs

import (
	"strings"
	"testing"
)

const fixture = `{"__REALTIME_TIMESTAMP":"1000000000","MESSAGE_ID":"7d4958e842da4a758f6c1cdc7b36dcc5","USER_UNIT":"sweep.service","USER_INVOCATION_ID":"aaa","MESSAGE":"Starting sweep.service..."}
{"__REALTIME_TIMESTAMP":"1000500000","_SYSTEMD_USER_UNIT":"sweep.service","_SYSTEMD_INVOCATION_ID":"aaa","PRIORITY":"6","MESSAGE":"removed 3 entries"}
{"__REALTIME_TIMESTAMP":"1001000000","MESSAGE_ID":"39f53479d3a045ac8e11786248231fbf","USER_UNIT":"sweep.service","USER_INVOCATION_ID":"aaa","JOB_RESULT":"done","MESSAGE":"Finished sweep.service."}
{"__REALTIME_TIMESTAMP":"1001000100","MESSAGE_ID":"ae8f7b866b0347b9af31fe1c80b127c0","USER_UNIT":"sweep.service","USER_INVOCATION_ID":"aaa","MESSAGE":"sweep.service: Consumed 1s CPU time."}
{"__REALTIME_TIMESTAMP":"2000000000","MESSAGE_ID":"7d4958e842da4a758f6c1cdc7b36dcc5","USER_UNIT":"check.service","USER_INVOCATION_ID":"bbb","MESSAGE":"Starting check.service..."}
{"__REALTIME_TIMESTAMP":"2000100000","_SYSTEMD_USER_UNIT":"check.service","_SYSTEMD_INVOCATION_ID":"bbb","PRIORITY":"3","MESSAGE":"Traceback: boom"}
{"__REALTIME_TIMESTAMP":"2000200000","MESSAGE_ID":"98e322203f7a4ed290d09fe03c09fe15","USER_UNIT":"check.service","USER_INVOCATION_ID":"bbb","EXIT_STATUS":"1","MESSAGE":"Main process exited, code=exited, status=1/FAILURE"}
{"__REALTIME_TIMESTAMP":"2000300000","MESSAGE_ID":"d9b373ed55a64feb8242e02dbe79a49c","USER_UNIT":"check.service","USER_INVOCATION_ID":"bbb","UNIT_RESULT":"exit-code","MESSAGE":"Failed with result 'exit-code'."}
{"__REALTIME_TIMESTAMP":"2000300100","MESSAGE_ID":"39f53479d3a045ac8e11786248231fbf","USER_UNIT":"check.service","USER_INVOCATION_ID":"bbb","JOB_RESULT":"failed","MESSAGE":"Failed to start check.service."}
{"__REALTIME_TIMESTAMP":"3000000000","MESSAGE_ID":"7d4958e842da4a758f6c1cdc7b36dcc5","USER_UNIT":"serve.service","USER_INVOCATION_ID":"ccc","MESSAGE":"Starting serve.service..."}
{"__REALTIME_TIMESTAMP":"3000100000","MESSAGE_ID":"39f53479d3a045ac8e11786248231fbf","USER_UNIT":"serve.service","USER_INVOCATION_ID":"ccc","JOB_RESULT":"done","MESSAGE":"Started serve.service."}
{"__REALTIME_TIMESTAMP":"3000200000","_SYSTEMD_USER_UNIT":"other.service","_SYSTEMD_INVOCATION_ID":"ddd","MESSAGE":"not a job"}
`

func TestAssemble(t *testing.T) {
	timers := map[string]string{"sweep.service": "sweep.timer", "check.service": "check.timer", "serve.service": "serve.timer"}
	runs := assemble(parseEntries([]byte(fixture)), "user", timers, map[string]bool{"serve.service": true}, 0)
	if len(runs) != 2 {
		t.Fatalf("want 2 finished runs (the running service waits), got %+v", runs)
	}
	ok, bad := runs[0], runs[1]
	if ok.Unit != "sweep.timer" || !ok.OK || ok.Summary != "removed 3 entries" || ok.Lines != 1 || ok.Start != 1000000 || ok.End != 1001000 {
		t.Errorf("success run: %+v", ok)
	}
	if bad.OK || bad.Result != "exit-code" || bad.ExitStatus != 1 || bad.Warnings != 1 || !strings.Contains(bad.Summary, "boom") {
		t.Errorf("failed run: %+v", bad)
	}
	// Once the service has stopped, its "Started" line closes the run.
	runs = assemble(parseEntries([]byte(fixture)), "user", timers, nil, 1500000)
	if len(runs) != 2 || runs[0].Unit != "check.timer" || runs[1].Unit != "serve.timer" {
		t.Fatalf("since filter / stopped service: %+v", runs)
	}
}

func TestShowParsing(t *testing.T) {
	out := "Id=a.timer\nTimersCalendar={ OnCalendar=*-*-* 04:15:00 ; next_elapse=Mon 2026-10-26 04:15:00 CET }\nTimersMonotonic={ OnBootUSec=15min ; next_elapse=15min }\nNextElapseUSecRealtime=@1792909080\n\nId=a.service\nExecMainStatus=0\n"
	b := parseShow([]byte(out))
	if len(b) != 2 || first(b[0], "Id") != "a.timer" || first(b[1], "Id") != "a.service" {
		t.Fatalf("blocks: %v", b)
	}
	if s := scheduleOf(first(b[0], "TimersCalendar")); s != "OnCalendar=*-*-* 04:15:00" {
		t.Errorf("calendar: %q", s)
	}
	if s := scheduleOf(first(b[0], "TimersMonotonic")); s != "OnBootUSec=15min" {
		t.Errorf("monotonic: %q", s)
	}
	if ms := unixMs(first(b[0], "NextElapseUSecRealtime")); ms != 1792909080000 {
		t.Errorf("unix: %d", ms)
	}
	if unixMs("n/a") != 0 || unixMs("") != 0 {
		t.Error("empty timestamps are 0")
	}
}

func TestExcludeAndAdmin(t *testing.T) {
	r := &Reader{Opts: Options{Exclude: []string{"apt-*", "fstrim"}}}
	if !r.excluded("apt-daily.timer") || !r.excluded("fstrim.timer") || r.excluded("backup.timer") {
		t.Error("exclude patterns")
	}
	if !adminMade("/etc/systemd/system/x.timer") || adminMade("/usr/lib/systemd/system/apt-daily.timer") {
		t.Error("admin-made")
	}
}

func TestPausedIsRemembered(t *testing.T) {
	f := t.TempDir() + "/state/paused.json"
	r := &Reader{Opts: Options{StateFile: f}, paused: map[string]bool{}}
	if err := r.setPaused("user", "a.timer", true); err != nil {
		t.Fatal(err)
	}
	r2, err := New(Options{StateFile: f})
	if err != nil {
		t.Skip(err) // no systemctl in this environment
	}
	if !r2.isPaused("user", "a.timer") || r2.isPaused("system", "a.timer") {
		t.Fatal("pause not read back")
	}
	_ = r2.setPaused("user", "a.timer", false)
	r3, _ := New(Options{StateFile: f})
	if r3.isPaused("user", "a.timer") {
		t.Fatal("resume not saved")
	}
}
