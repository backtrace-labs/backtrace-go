package bt

import (
	"encoding/json"
	"reflect"
	"testing"
)

const stackExample1 = `goroutine 1 [running]:
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
`

func TestParseThreadsFromStack(t *testing.T) {
	t.Run("example1_ok", func(t *testing.T) {
		expectedThreads := map[string]interface{}{
			"1": map[string]interface{}{
				"name": "goroutine 1 [running]",
				"stack": []map[string]interface{}{
					{
						"funcName":   "GetStack",
						"library":    "main",
						"line":       30,
						"sourceCode": "0",
					},
					{
						"funcName":   "main",
						"library":    "main",
						"line":       15,
						"sourceCode": "0",
					},
				},
			},

			"7": map[string]interface{}{
				"name": "goroutine 6 [runnable]",
				"stack": []map[string]interface{}{
					{
						"funcName":   "testFunc",
						"library":    "main",
						"line":       22,
						"sourceCode": "0",
					},
					{
						"funcName":   "main",
						"library":    "main",
						"line":       13,
						"sourceCode": "0",
					},
				},
			},

			"13": map[string]interface{}{
				"name": "goroutine 7 [runnable]",
				"stack": []map[string]interface{}{
					{
						"funcName":   "testFunc",
						"library":    "main",
						"line":       22,
						"sourceCode": "0",
					},
					{
						"funcName":   "main",
						"library":    "main",
						"line":       14,
						"sourceCode": "0",
					},
				},
			},
		}
		expectedMainThread := "1"
		expectedSourceCode := map[string]interface{}{
			"0": map[string]interface{}{
				"path": "/tmp/sandbox889685435/prog.go",
			},
		}

		threads, mainThread, sourceCode, err := ParseThreadsFromStack([]byte(stackExample1))
		requireNoErr(t, err)
		requireJSONEqual(t, expectedThreads, threads)
		requireEqual(t, expectedMainThread, mainThread)
		requireEqual(t, expectedSourceCode, sourceCode)
	})
}

func requireJSONEqual(t *testing.T, expectedVal interface{}, val interface{}) {
	expectedValJSON, err := json.Marshal(expectedVal)
	requireNoErr(t, err)
	valJSON, err := json.Marshal(val)
	requireNoErr(t, err)

	expectedValJSONStr := string(expectedValJSON)
	valJSONStr := string(valJSON)

	if expectedValJSONStr != valJSONStr {
		t.Fatalf("unexpected JSON inequality; following values are not equal:\n%v\n%v", expectedValJSONStr, valJSONStr)
	}
}

func requireEqual(t *testing.T, expectedVal interface{}, val interface{}) {
	if !reflect.DeepEqual(expectedVal, val) {
		t.Fatalf("unexpected inequality; following values are not equal:\n%v (%T)\n%v (%T)", expectedVal, expectedVal, val, val)
	}
}

func requireNoErr(t *testing.T, err error) {
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
