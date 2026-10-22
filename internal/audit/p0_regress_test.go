package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// P0 回归 1+4：批内部分失败时，result 必须逐条反映真实物理结果——
// 成功的文件记 ok:true，失败的文件记 ok:false + 该文件自己的错误文本。
// 旧缺陷：action 返回首个错误后，CommitMoves 把整批 256 条全记 ok:false
// 且 err 文本取首个错误盖整批（审计无法按 result.err 归因单文件）。
//
// P0 回归 4：失败的 result 行必须显式含 "ok":false（旧 omitempty 吞掉）；
// intent 行不得含 ok 字段。
func TestCommitMovesPartialFailurePerFileResults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	jl, err := OpenJSONL(path)
	if err != nil {
		t.Fatal(err)
	}
	defer jl.Close()

	errF2 := errors.New("rename /a/f2: permission denied")
	errF4 := errors.New("recheck: inode changed")

	// 批内 4 条：f1 成功、f2 失败、f3 成功、f4 失败。
	// 新契约：action 返回 per-file 结果（nil=成功）。
	// 旧契约兼容注入：单 error 时旧实现整批 false——本测试在新签名下编译。
	perFile := []error{nil, errF2, nil, errF4}
	err = jl.CommitMoves([]MoveIntent{
		{RunID: "r1", PolicyID: "p1", Src: "/a/f1", Trash: "/t/f1", Inode: 1},
		{RunID: "r1", PolicyID: "p1", Src: "/a/f2", Trash: "/t/f2", Inode: 2},
		{RunID: "r1", PolicyID: "p1", Src: "/a/f3", Trash: "/t/f3", Inode: 3},
		{RunID: "r1", PolicyID: "p1", Src: "/a/f4", Trash: "/t/f4", Inode: 4},
	}, func() []error { return perFile })
	if err == nil || err.Error() != errF2.Error() {
		t.Fatalf("CommitMoves aggregate err = %v, want first non-nil (%v)", err, errF2)
	}
	jl.Sync()

	results := readResults(t, path)
	if len(results) != 4 {
		t.Fatalf("result events = %d, want 4", len(results))
	}
	wantOK := []bool{true, false, true, false}
	wantErr := []string{"", errF2.Error(), "", errF4.Error()}
	for i, e := range results {
		if e.OK == nil {
			t.Fatalf("result[%d] (src=%s): ok field missing entirely", i, e.Src)
		}
		if *e.OK != wantOK[i] {
			t.Errorf("result[%d] (src=%s): ok=%v, want %v (per-file result must reflect physical outcome, not batch outcome)", i, e.Src, *e.OK, wantOK[i])
		}
		if e.Err != wantErr[i] {
			t.Errorf("result[%d] (src=%s): err=%q, want %q (err must be attributed to this file, not the first batch error)", i, e.Src, e.Err, wantErr[i])
		}
	}

	// 序列化契约：失败行必须显式含 "ok":false；成功行显式 "ok":true；
	// intent 行不得含 ok 字段。
	rawLines := readRawLines(t, path)
	for i, line := range rawLines {
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(line), &probe) != nil {
			t.Fatalf("line[%d] not valid json: %s", i, line)
		}
		if probe.Type == EventResult {
			if !strings.Contains(line, `"ok":false`) && !strings.Contains(line, `"ok":true`) {
				t.Errorf("result line lacks explicit ok field (omitempty bug): %s", line)
			}
		}
		if probe.Type == EventIntent && strings.Contains(line, `"ok"`) {
			t.Errorf("intent line must not carry ok field: %s", line)
		}
	}
}

// P0 回归 4（独立最小断言）：WriteRestoreResult(ok=false) 落盘行必须
// 显式含 "ok":false——审计取证 grep '"ok":false' 必须能命中。
func TestFailureEventsSerializeOKFalse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	jl, _ := OpenJSONL(path)
	defer jl.Close()

	if err := jl.WritePurgeResult("r1", "p1", "/t/f1", false, "unlink: eacces"); err != nil {
		t.Fatal(err)
	}
	if err := jl.WriteRestoreResult("r1", "p1", "/t/f1", "/a/f1", false, "rename: enoent"); err != nil {
		t.Fatal(err)
	}
	jl.Sync()

	for _, line := range readRawLines(t, path) {
		if !strings.Contains(line, `"ok":false`) {
			t.Errorf("failed result must serialize explicit ok:false: %s", line)
		}
	}
}

// ---- helpers ----

func readResults(t *testing.T, path string) []Event {
	t.Helper()
	var out []Event
	for _, line := range readRawLines(t, path) {
		var e Event
		if json.Unmarshal([]byte(line), &e) == nil && e.Type == EventResult {
			out = append(out, e)
		}
	}
	return out
}

func readRawLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}
