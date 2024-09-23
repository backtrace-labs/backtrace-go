package bt

import (
	"fmt"
	"os"
	"strings"
)

type Thread struct {
	Name   string       `json:"name"`
	Stacks []StackFrame `json:"stack"`
}

type StackFrame struct {
	FuncName      string `json:"funcName"`
	Library       string `json:"library"`
	SourceCodeID  string `json:"sourceCode"`
	Line          string `json:"line"`
	skipBacktrace bool
}

type SourceCode struct {
	Text        string `json:"text"`
	Path        string `json:"path"`
	StartLine   int    `json:"startLine"`
	StartColumn int    `json:"startColumn"`
	StartPos    int    `json:"startPos"`
	TabWidth    int    `json:"tabWidth"`
}

func ParseThreadsFromStack(stackTrace []byte) (map[string]Thread, map[string]SourceCode) {
	splitThreads := strings.Split(string(stackTrace), "\n\n")

	sourceCodeID := 0

	sourcesPath := make(map[string]int)        // key: path, value: unique path number starting from 0.
	threads := make(map[string]Thread)         // key: index of split string, starting from 0.
	sourceCodes := make(map[string]SourceCode) // key: unique path number starting from 0.

	for threadID, stackText := range splitThreads {
		lines := strings.Split(stackText, "\n")

		sf := StackFrame{}
		thread := Thread{Name: strings.TrimSuffix(lines[0], ":")}
		for i := 1; i < len(lines); i++ {
			line := strings.TrimSpace(lines[i])
			if line == "" {
				continue
			}

			if i%2 != 0 { // odd lines are function paths
				line = trimCreatedBy(line)
				if strings.HasPrefix(line, "github.com/backtrace-labs/backtrace-go") {
					sf.skipBacktrace = true
					continue
				}
				sf.skipBacktrace = false

				lastIndex, function := getLastPathIndexAndFunction(line)

				if function == "panic" {
					sf.FuncName = "panic"
					sf.Library = "runtime"
					continue
				}

				sf.FuncName = function
				sf.Library = line[:lastIndex]
			} else {
				if sf.skipBacktrace {
					continue
				}

				line = strings.TrimSpace(line)
				line, _, _ = strings.Cut(line, " +")

				path := ""
				path, sf.Line, _ = strings.Cut(line, ":")

				if scID, ok := sourcesPath[path]; ok {
					sf.SourceCodeID = fmt.Sprintf("%d", scID)
				} else {
					strSourceCodeID := fmt.Sprintf("%d", sourceCodeID)
					sourcesPath[path] = sourceCodeID

					sourceCodes[strSourceCodeID] = readFileGetSourceCode(path)

					sf.SourceCodeID = strSourceCodeID

					sourceCodeID++
				}
				thread.Stacks = append(thread.Stacks, sf)
				sf = StackFrame{}
			}
		}

		if len(thread.Stacks) > 0 {
			threads[fmt.Sprintf("%d", threadID)] = thread
		}
	}

	return threads, sourceCodes
}

func readFileGetSourceCode(path string) SourceCode {
	sc := SourceCode{}
	bytes, err := os.ReadFile(path)
	if err == nil {
		sc.Text = string(bytes)
		sc.StartLine = 1
		sc.StartColumn = 1
		sc.StartPos = 0
		sc.TabWidth = Options.TabWidth
	}
	sc.Path = path

	return sc
}

func getLastPathIndexAndFunction(line string) (int, string) {
	lastIndex := strings.LastIndex(line, ".")
	function, _, _ := strings.Cut(line[lastIndex+1:], "(")
	return lastIndex, function
}

func trimCreatedBy(line string) string {
	if strings.HasPrefix(line, "created by") {
		_, line, _ = strings.Cut(line, " by ")
		line, _, _ = strings.Cut(line, " in ")
	}
	return line
}
