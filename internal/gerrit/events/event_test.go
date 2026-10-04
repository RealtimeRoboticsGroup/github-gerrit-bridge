package events

import (
	"testing"
)

// A 3.x patchset-created event, trimmed, with fields this package does not
// know about (gerrit-buildkite's DisallowUnknownFields would reject it).
const patchsetCreated = `{"uploader":{"name":"Austin Schuh","email":"austin@example.com","username":"austin"},
"patchSet":{"number":3,"revision":"f00df00df00df00df00df00df00df00df00df00d","parents":["aaaa"],
"ref":"refs/changes/45/12345/3","uploader":{"name":"Austin Schuh","username":"austin"},
"createdOn":1759500000,"author":{"name":"Austin Schuh"},"kind":"REWORK","sizeInsertions":10,"sizeDeletions":-2},
"change":{"project":"frc971/971-Robot-Code","branch":"main","id":"I0123456789abcdef0123456789abcdef01234567",
"number":12345,"subject":"Do the thing","owner":{"name":"Austin Schuh"},"url":"https://g/c/12345",
"commitMessage":"Do the thing\n","createdOn":1759400000,"status":"NEW","wip":true,"brandNewField":{"x":1}},
"project":"frc971/971-Robot-Code","refName":"refs/heads/main","changeKey":{"key":"I0123"},
"type":"patchset-created","eventCreatedOn":1759500001}`

func TestParseLenient(t *testing.T) {
	e, err := Parse([]byte(patchsetCreated))
	if err != nil {
		t.Fatal(err)
	}
	if e.Type != "patchset-created" || e.CreatedOn != 1759500001 {
		t.Errorf("type/time = %q %d", e.Type, e.CreatedOn)
	}
	if e.Change.Number != 12345 || !e.Change.WIP || e.Change.ID != "I0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("change = %+v", e.Change)
	}
	if e.PatchSet.Number != 3 || e.PatchSet.Kind != "REWORK" {
		t.Errorf("patchSet = %+v", e.PatchSet)
	}
	if e.Project() != "frc971/971-Robot-Code" {
		t.Errorf("Project = %q", e.Project())
	}
	if a := e.Actor(); a == nil || a.Username != "austin" {
		t.Errorf("Actor = %+v", a)
	}
	if len(e.Raw) == 0 {
		t.Error("Raw not kept")
	}
}

func TestParseRefUpdated(t *testing.T) {
	e, err := Parse([]byte(`{"submitter":{"name":"Bot"},"refUpdate":{"oldRev":"a","newRev":"b",
"refName":"refs/heads/main","project":"p"},"type":"ref-updated","eventCreatedOn":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if e.Project() != "p" || e.RefUpdate.RefName != "refs/heads/main" || e.Change != nil {
		t.Errorf("got %+v %+v", e, e.RefUpdate)
	}
}

func TestParseSalvagesChangeNumber(t *testing.T) {
	// "patchSet.number" changed type: the event cannot fully decode, but
	// the change number is enough to schedule a reconcile.
	e, err := Parse([]byte(`{"type":"patchset-created","change":{"project":"p","number":"77"},
"patchSet":{"number":{"weird":true}}}`))
	if err == nil {
		t.Fatal("expected a partial-decode error")
	}
	if e == nil || e.Type != "patchset-created" || e.Change == nil || e.Change.Number != 77 || e.Project() != "p" {
		t.Fatalf("salvaged %+v", e)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, in := range []string{``, `not json`, `{}`, `{"type":""}`, `[1,2]`} {
		if e, err := Parse([]byte(in)); err == nil || e != nil {
			t.Errorf("Parse(%q) = %+v, %v; want error", in, e, err)
		}
	}
}

func TestCommand(t *testing.T) {
	got := Command([]string{"patchset-created", "dropped-output"})
	want := "gerrit stream-events -s patchset-created -s dropped-output"
	if got != want {
		t.Errorf("Command = %q, want %q", got, want)
	}
}
