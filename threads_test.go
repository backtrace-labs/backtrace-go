package bt

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

const stackTrace = `goroutine 1 [running]:
github.com/backtrace-labs/backtrace-go.TestMain(0x1400011a960)
	/Users/test-user/Documents/Work/backtrace-go/main_test.go:41 +0x28
main.GetStack()
    /tmp/sandbox889685435/prog.go:30 +0x5f
main.main()
    /tmp/sandbox889685435/prog.go:15 +0x2f

goroutine 6 [runnable]:
main.testFunc()
    /tmp/sandbox889685435/prog.go:22
created by main.main in goroutine 1
    /tmp/sandbox889685435/prog.go:13 +0x1e

goroutine 7 [runnable]:
main.testFunc()
    /tmp/sandbox889685435/prog.go:22
created by main.main in goroutine 1
    /tmp/sandbox889685435/prog.go:14 +0x2a

goroutine 8 [running]:
main.test.main(0x9)
        /Users/root/Library/Application Support/JetBrains/GoLand2024.1/scratches/scratch_19.go:74 +0xa0c

goroutine 9 [running]:
testing.(*T).Run(0x14000110680, {0x1012116ab, 0x9}, 0x1012ff428)
        /Users/some_file.go:12 +0xa0c
panic({0x100b3f360?, 0x100bb0860?})
	/usr/local/go/src/runtime/something.go:770 +0xf0
created by testing.(*T).Run in goroutine 1
	/usr/local/go/src/testing/foobar.go:1742 +0x668
`

func TestParseThreadsFromStack(t *testing.T) {
	type args struct {
		stackTrace []byte
	}
	tests := []struct {
		name           string
		args           args
		wantThreads    map[string]Thread
		wantSourceCode map[string]SourceCode
	}{
		{
			name: "ShouldParseThreadsFromStack",
			args: args{
				stackTrace: []byte(stackTrace),
			},
			wantThreads: map[string]Thread{
				"0": {
					Name:  "goroutine 1 [running]",
					Fault: true,
					Stacks: []StackFrame{
						{
							FuncName:     "GetStack",
							Library:      "main",
							SourceCodeID: "0",
							Line:         "30",
						},
						{
							FuncName:     "main",
							Library:      "main",
							SourceCodeID: "0",
							Line:         "15",
						},
					},
				},
				"1": {
					Name: "goroutine 6 [runnable]",
					Stacks: []StackFrame{
						{
							FuncName:     "testFunc",
							Library:      "main",
							SourceCodeID: "0",
							Line:         "22",
						},
						{
							FuncName:     "main",
							Library:      "main",
							SourceCodeID: "0",
							Line:         "13",
						},
					},
				},
				"2": {
					Name: "goroutine 7 [runnable]",
					Stacks: []StackFrame{
						{
							FuncName:     "testFunc",
							Library:      "main",
							SourceCodeID: "0",
							Line:         "22",
						},
						{
							FuncName:     "main",
							Library:      "main",
							SourceCodeID: "0",
							Line:         "14",
						},
					},
				},
				"3": {
					Name: "goroutine 8 [running]",
					Stacks: []StackFrame{
						{
							FuncName:     "main",
							Library:      "main.test",
							SourceCodeID: "1",
							Line:         "74",
						},
					},
				},
				"4": {
					Name: "goroutine 9 [running]",
					Stacks: []StackFrame{
						{
							FuncName:     "Run",
							Library:      "testing.(*T)",
							SourceCodeID: "2",
							Line:         "12",
						},
						{
							FuncName:     "panic",
							Library:      "runtime",
							SourceCodeID: "3",
							Line:         "770",
						},
						{
							FuncName:     "Run",
							Library:      "testing.(*T)",
							SourceCodeID: "4",
							Line:         "1742",
						},
					},
				},
			},
			wantSourceCode: map[string]SourceCode{
				"0": {
					Path: "/tmp/sandbox889685435/prog.go",
				},
				"1": {
					Path: "/Users/root/Library/Application Support/JetBrains/GoLand2024.1/scratches/scratch_19.go",
				},
				"2": {
					Path: "/Users/some_file.go",
				},
				"3": {
					Text: "",
					Path: "/usr/local/go/src/runtime/something.go",
				},
				"4": {
					Text: "",
					Path: "/usr/local/go/src/testing/foobar.go",
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotThreads, gotSourceCodes := ParseThreadsFromStack(tt.args.stackTrace)

			waitChan := make(chan int)
			go func() {
				for k, v := range gotSourceCodes {
					gotSourceCodes[k] = SourceCode{
						Text:        "", // remove text check.
						Path:        v.Path,
						StartLine:   v.StartLine,
						StartColumn: v.StartColumn,
						StartPos:    v.StartPos,
						TabWidth:    v.TabWidth,
					}
				}
				waitChan <- 1
			}()
			<-waitChan

			assert.Equal(t, tt.wantThreads, gotThreads)
			assert.Equal(t, tt.wantSourceCode, gotSourceCodes)
		})
	}
}
