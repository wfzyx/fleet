package agent

import (
	"errors"
	"strings"
	"testing"
)

const goodVerdict = `{"verdict":"approve","summary":"Looks good.","criteria":[{"criterion":"c1","met":true,"evidence":"x.go:1"}],"issues":[{"severity":"nit","message":"naming"}]}`

func TestParseReviewVerdict(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Verdict
		wantErr error
	}{
		{"bare", goodVerdict, VerdictApprove, nil},
		{"fenced", "Here you go:\n```json\n" + goodVerdict + "\n```\nDone.", VerdictApprove, nil},
		{"stray brace in prose", "I used { to start. " + goodVerdict, VerdictApprove, nil},
		{"uppercase verdict", strings.Replace(goodVerdict, `"approve"`, `"APPROVE"`, 1), VerdictApprove, nil},
		{"last object wins", `{"verdict":"reject","summary":"first"} then ` + goodVerdict, VerdictApprove, nil},
		{"braces in strings", `{"verdict":"request_changes","summary":"use } carefully {","issues":[{"severity":"major","message":"a \"}\" b"}]}`, VerdictRequestChanges, nil},
		{"request changes", `{"verdict":"request_changes","summary":"s","criteria":[{"criterion":"c","met":false}],"issues":[{"severity":"blocker","file":"a.go","line":3,"message":"m"}]}`, VerdictRequestChanges, nil},
		{"no json", "looks fine to me", "", ErrNoVerdict},
		{"empty", "", "", ErrNoVerdict},
		{"json without verdict", `{"foo":1}`, "", ErrNoVerdict},
		{"unterminated", `{"verdict":"approve"`, "", ErrNoVerdict},
		{"unknown verdict", `{"verdict":"maybe","summary":"s"}`, "", ErrInvalidVerdict},
		{"missing summary", `{"verdict":"approve"}`, "", ErrInvalidVerdict},
		{"wrong types", `{"verdict":"approve","summary":5}`, "", ErrInvalidVerdict},
		{"approve with unmet", `{"verdict":"approve","summary":"s","criteria":[{"criterion":"c","met":false}]}`, "", ErrInvalidVerdict},
		{"approve with blocker", `{"verdict":"approve","summary":"s","issues":[{"severity":"blocker","message":"m"}]}`, "", ErrInvalidVerdict},
		{"bad severity", `{"verdict":"reject","summary":"s","issues":[{"severity":"bad","message":"m"}]}`, "", ErrInvalidVerdict},
		{"issue without message", `{"verdict":"reject","summary":"s","issues":[{"severity":"minor"}]}`, "", ErrInvalidVerdict},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := ParseReviewVerdict(tc.in)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if v.Verdict != tc.want {
				t.Errorf("verdict = %q, want %q", v.Verdict, tc.want)
			}
		})
	}
}

func TestParseReviewVerdictFields(t *testing.T) {
	v, err := ParseReviewVerdict(`{"verdict":"request_changes","summary":"s","criteria":[{"criterion":"c","met":false,"evidence":"e"}],"issues":[{"severity":"major","file":"a.go","line":7,"message":"m"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if v.Approved() {
		t.Error("request_changes is not approved")
	}
	if len(v.Criteria) != 1 || v.Criteria[0].Met || v.Criteria[0].Evidence != "e" {
		t.Errorf("criteria = %+v", v.Criteria)
	}
	if len(v.Issues) != 1 || v.Issues[0] != (Issue{Severity: SeverityMajor, File: "a.go", Line: 7, Message: "m"}) {
		t.Errorf("issues = %+v", v.Issues)
	}
	a, _ := ParseReviewVerdict(goodVerdict)
	if !a.Approved() {
		t.Error("approve should be approved")
	}
}

func TestVerdictSchemaMentionsAllVerdicts(t *testing.T) {
	for _, v := range []Verdict{VerdictApprove, VerdictRequestChanges, VerdictReject} {
		if !strings.Contains(VerdictSchema, string(v)) {
			t.Errorf("schema lacks %q", v)
		}
	}
}
