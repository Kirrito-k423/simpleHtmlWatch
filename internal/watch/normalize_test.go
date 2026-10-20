package watch

import "testing"

func TestNormalizeWatch(t *testing.T) {
	for input, want := range map[string]string{"watch npu-smi info": "npu-smi info", "watch -n 4 'ps -ef | grep tilexr'": "ps -ef | grep tilexr", "/usr/bin/watch --interval=2 -d -- ps -ef": "ps -ef", "printf '%s' \"hello\"": "printf '%s' \"hello\""} {
		got, e := NormalizeWatch(input)
		if e != nil || got != want {
			t.Fatalf("%q => %q %v", input, got, e)
		}
	}
	for _, s := range []string{"watch", "watch --exec ps -ef", "watch 'unterminated", "watch 'a' && 'b'", "watch watch ps -ef"} {
		if _, e := NormalizeWatch(s); e == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}
