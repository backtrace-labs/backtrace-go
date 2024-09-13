package bt

import (
	"bytes"
	"io/ioutil"
	"regexp"
	"strconv"
)

var (
	threadRegex      = regexp.MustCompile(`^(.*):$`)
	fnRegex          = regexp.MustCompile(`^(.*)\.([^(]+)`)
	createdByFnRegex = regexp.MustCompile(`^created by (.*)\.([^ ]+) in goroutine [0-9]+$`)
	srcRegex         = regexp.MustCompile(`^\s*(.*):(\d+)`)
)

type thread struct {
	id     int
	name   string
	frames []frame
}

type frame struct {
	fnName     string
	library    string
	sourcePath string
	line       int
}

func ParseThreadsFromStack(goStack []byte) (map[string]interface{}, string, map[string]interface{}, error) {
	lines := bytes.Split(goStack, []byte{'\n'})

	sourcePathToID := map[string]string{}
	sourceCode := map[string]interface{}{}

	threads := map[string]interface{}{}
	var mainThread string

	nextSourceID := 0
	lineIndex := 0
	first := true
	for lineIndex < len(lines) {
		threadItem := getThread(lines, &lineIndex)
		if threadItem == nil {
			break
		}
		threadID := strconv.Itoa(threadItem.id)
		if first {
			first = false
			mainThread = threadID
		}
		stackList := []interface{}{}
		for _, frameItem := range threadItem.frames {
			sourceCodeID := collectSource(frameItem.sourcePath, sourcePathToID, sourceCode, &nextSourceID)

			stackFrame := map[string]interface{}{}
			stackFrame["funcName"] = frameItem.fnName
			stackFrame["library"] = frameItem.library
			stackFrame["sourceCode"] = sourceCodeID
			stackFrame["line"] = frameItem.line
			stackList = append(stackList, stackFrame)
		}

		threadMap := map[string]interface{}{}
		threadMap["name"] = threadItem.name
		threadMap["stack"] = stackList
		threads[threadID] = threadMap
	}

	return threads, mainThread, sourceCode, nil
}

func collectSource(sourcePath string, sourcePathToID map[string]string, sourceCodeJSON map[string]interface{}, nextSourceID *int) string {
	existingID, present := sourcePathToID[sourcePath]
	if present {
		return existingID
	}
	newID := strconv.Itoa(*nextSourceID)
	*nextSourceID += 1
	sourcePathToID[sourcePath] = newID

	sourceCodeObject := map[string]interface{}{}

	bytes, err := ioutil.ReadFile(sourcePath)
	if err == nil {
		sourceCodeObject["text"] = string(bytes)
		sourceCodeObject["startLine"] = 1
		sourceCodeObject["startColumn"] = 1
		sourceCodeObject["startPos"] = 0
		sourceCodeObject["tabWidth"] = Options.TabWidth
	}
	sourceCodeObject["path"] = sourcePath

	sourceCodeJSON[newID] = sourceCodeObject

	return newID
}

func getThread(lines [][]byte, index *int) *thread {
	threadLine := getNextLine(lines, index)
	if threadLine == nil {
		return nil
	}
	threadMatches := threadRegex.FindSubmatch(threadLine)
	if threadMatches == nil {
		return nil
	}
	threadItem := thread{
		id:   *index,
		name: string(threadMatches[1]),
	}
	for {
		fnLine := getNextLine(lines, index)
		if fnLine == nil {
			break
		}
		srcLine := getNextLine(lines, index)
		if srcLine == nil {
			break
		}

		fnNameMatches := fnRegex.FindSubmatch(fnLine)
		createdByFnNameMatches := createdByFnRegex.FindSubmatch(fnLine)
		sourcePathMatches := srcRegex.FindSubmatch(srcLine)

		newFrame := frame{}
		if createdByFnNameMatches != nil {
			newFrame.library = string(createdByFnNameMatches[1])
			newFrame.fnName = string(createdByFnNameMatches[2])

		} else if fnNameMatches != nil {
			newFrame.library = string(fnNameMatches[1])
			newFrame.fnName = string(fnNameMatches[2])
		}
		if sourcePathMatches != nil {
			newFrame.sourcePath = string(sourcePathMatches[1])
			newFrame.line, _ = strconv.Atoi(string(sourcePathMatches[2]))
		}

		threadItem.frames = append(threadItem.frames, newFrame)
	}
	return &threadItem
}

func getNextLine(lines [][]byte, index *int) []byte {
	if *index < len(lines) && len(lines[*index]) != 0 {
		result := lines[*index]
		*index += 1
		return result
	}
	*index += 1
	return nil
}
