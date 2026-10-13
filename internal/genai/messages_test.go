package genai

import "testing"

func TestParseMessagesShapes(t *testing.T) {
	in := `[{"role":"system","content":"[lookout: 3 earlier items truncated, 900 bytes]"},
{"role":"user","parts":[{"type":"text","content":"hi"},{"type":"text","text":"there"}]},
{"role":"assistant","parts":[{"type":"tool_call","id":"c1","name":"Bash","arguments":{"cmd":"ls"}}]},
{"role":"user","parts":[{"type":"tool_result","id":"c1","content":[{"type":"text","text":"a.txt"}]}]},
{"role":"tool","tool_call_id":"c2","content":"plain result"},
{"role":"tool","parts":[{"type":"tool_call_response","id":"c3","response":"resp"}]}]`
	msgs, ok := ParseMessages(in)
	if !ok || len(msgs) != 6 {
		t.Fatalf("ok=%v n=%d", ok, len(msgs))
	}
	if !msgs[0].Marker || msgs[0].Role != "system" {
		t.Errorf("marker not detected: %+v", msgs[0])
	}
	if msgs[1].Text() != "hi\nthere" || !msgs[1].HasUserText() {
		t.Errorf("text parts (content+text keys): %q", msgs[1].Text())
	}
	if p := msgs[2].Parts[0]; p.Kind != PartToolCall || p.Name != "Bash" || p.ID != "c1" || p.Args != `{"cmd":"ls"}` {
		t.Errorf("tool_call: %+v", p)
	}
	if p := msgs[3].Parts[0]; p.Kind != PartToolResult || p.Text != "a.txt" || p.ID != "c1" || msgs[3].HasUserText() {
		t.Errorf("tool_result: %+v", p)
	}
	if p := msgs[4].Parts[0]; p.Kind != PartToolResult || p.Text != "plain result" || p.ID != "c2" {
		t.Errorf("openai tool message: %+v", p)
	}
	if p := msgs[5].Parts[0]; p.Kind != PartToolResult || p.Text != "resp" {
		t.Errorf("tool_call_response: %+v", p)
	}
}

func TestParseMessagesNotAnArray(t *testing.T) {
	for _, s := range []string{"", "plain text…[truncated 12 bytes]", `{"a":1}`, `[{"role":"user","parts":[{"type":"te…[truncated 5 bytes]`} {
		if _, ok := ParseMessages(s); ok {
			t.Errorf("%q should not parse", s)
		}
	}
}
