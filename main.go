package bt

import "bytes"
import crypto_rand "crypto/rand"
import "encoding/json"
import "fmt"
import "io/ioutil"
import math_rand "math/rand"
import "os"
import "regexp"
import "runtime"
import "strconv"
import "strings"
import "time"

const VersionMajor = 0
const VersionMinor = 0
const VersionPatch = 0

var Version = fmt.Sprintf("%d.%d.%d", VersionMajor, VersionMinor, VersionPatch)

type OptionsStruct struct {
	Endpoint string
	Token    string

	CaptureAllGoroutines bool
	TabWidth             int
	ContextLineCount     int
}

var Options OptionsStruct

// TODO double check concurrency safety for all these variables
var thread_regex *regexp.Regexp
var fn_regex *regexp.Regexp
var src_regex *regexp.Regexp
var rng *math_rand.Rand

func init() {
	var err error
	thread_regex, err = regexp.Compile(`^(.*):$`)
	if err != nil {
		panic(err)
	}
	fn_regex, err = regexp.Compile(`^(created by )?(.*)\.([^(]+)`)
	if err != nil {
		panic(err)
	}
	src_regex, err = regexp.Compile(`^\s*(.*):(\d+)`)
	if err != nil {
		panic(err)
	}

	var seed_bytes [8]byte
	_, err = crypto_rand.Read(seed_bytes[:])
	if err != nil {
		panic(err)
	}

	seed :=
		(int64(seed_bytes[0]) << 0) |
			(int64(seed_bytes[1]) << 1) |
			(int64(seed_bytes[2]) << 2) |
			(int64(seed_bytes[3]) << 3) |
			(int64(seed_bytes[4]) << 4) |
			(int64(seed_bytes[5]) << 5) |
			(int64(seed_bytes[6]) << 6) |
			(int64(seed_bytes[7]) << 7)

	rand_source := math_rand.NewSource(seed)
	rng = math_rand.New(rand_source)
}

type thread struct {
	id     int
	name   string
	frames []frame
}

type frame struct {
	fn_name     string
	library     string
	source_path string
	line        int
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

func getThread(lines [][]byte, index *int) *thread {
	thread_line := getNextLine(lines, index)
	if thread_line == nil {
		return nil
	}
	thread_matches := thread_regex.FindSubmatch(thread_line)
	if thread_matches == nil {
		return nil
	}
	thread_item := thread{
		id:   *index,
		name: string(thread_matches[1]),
	}
	for {
		fn_line := getNextLine(lines, index)
		if fn_line == nil {
			break
		}
		src_line := getNextLine(lines, index)
		if src_line == nil {
			break
		}

		fn_name_matches := fn_regex.FindSubmatch(fn_line)
		source_path_matches := src_regex.FindSubmatch(src_line)

		new_frame := frame{}
		if fn_name_matches != nil {
			new_frame.library = string(fn_name_matches[2])
			new_frame.fn_name = string(fn_name_matches[3])
		}
		if source_path_matches != nil {
			new_frame.source_path = string(source_path_matches[1])
			new_frame.line, _ = strconv.Atoi(string(source_path_matches[2]))
		}

		thread_item.frames = append(thread_item.frames, new_frame)
	}
	return &thread_item
}

func SendReport(user_err error, extra_attributes map[string]string) {
	checkOptions()

	stack := stack(Options.CaptureAllGoroutines)
	fmt.Printf("stack: %s\n", stack)
	lines := bytes.Split(stack, []byte{'\n'})

	source_path_to_id := map[string]string{}
	source_code := map[string]interface{}{}

	attributes := map[string]interface{}{}
	attributes["error.message"] = user_err.Error()

	for k, v := range extra_attributes {
		attributes[k] = v
	}

	annotations := map[string]interface{}{}
	annotations["Environment Variables"] = getEnvVars()

	threads := map[string]interface{}{}

	report := map[string]interface{}{}
	report["uuid"] = createUuid()
	report["timestamp"] = time.Now().Unix()
	report["lang"] = "go"
	report["langVersion"] = runtime.Version()
	report["agent"] = "backtrace-go"
	report["agentVersion"] = Version
	report["attributes"] = attributes
	report["threads"] = threads
	report["sourceCode"] = source_code
	report["annotations"] = annotations

	next_source_id := 0
	line_index := 0
	first := true
	for line_index < len(lines) {
		thread_item := getThread(lines, &line_index)
		if thread_item == nil {
			break
		}
		thread_id := strconv.Itoa(thread_item.id)
		if first {
			first = false
			report["mainThread"] = thread_id
		}
		stack_list := []interface{}{}
		for _, frame_item := range thread_item.frames {
			source_code_id := collectSource(frame_item.source_path, source_path_to_id, source_code, &next_source_id)

			stack_frame := map[string]interface{}{}
			stack_frame["funcName"] = frame_item.fn_name
			stack_frame["library"] = frame_item.library
			stack_frame["sourceCode"] = source_code_id
			stack_frame["line"] = frame_item.line
			stack_list = append(stack_list, stack_frame)
		}

		thread_map := map[string]interface{}{}
		threads[thread_id] = thread_map
		thread_map["name"] = thread_item.name
		thread_map["stack"] = stack_list
	}

	var err error
	payload, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		panic(err)
	}
	fmt.Println(string(payload))
}

func stack(all bool) []byte {
	buf := make([]byte, 1024)
	for {
		n := runtime.Stack(buf, all)
		if n < len(buf) {
			return buf[:n]
		}
		buf = make([]byte, 2*len(buf))
	}
}

func getEnvVars() map[string]string {
	lines := os.Environ()
	result := map[string]string{}
	for _, line := range lines {
		kv := strings.Split(line, "=")
		result[kv[0]] = kv[1]
	}
	return result
}

func checkOptions() {
	if len(Options.Endpoint) == 0 {
		panic("must set bt.Options.Endpoint")
	}
	if len(Options.Token) == 0 {
		panic("must set bt.Options.Token")
	}
}

func collectSource(source_path string, source_path_to_id map[string]string, source_code_json map[string]interface{}, next_source_id *int) string {
	existing_id, present := source_path_to_id[source_path]
	if present {
		return existing_id
	}
	new_id := strconv.Itoa(*next_source_id)
	*next_source_id += 1
	source_path_to_id[source_path] = new_id

	source_code_object := map[string]interface{}{}

	bytes, err := ioutil.ReadFile(source_path)
	if err == nil {
		source_code_object["text"] = string(bytes)
		source_code_object["startLine"] = 1
		source_code_object["startColumn"] = 1
		source_code_object["startPos"] = 0
		source_code_object["tabWidth"] = Options.TabWidth
	}
	source_code_object["path"] = source_path

	source_code_json[new_id] = source_code_object

	return new_id
}

func createUuid() string {
	var uuid_bytes [16]byte
	_, _ = rng.Read(uuid_bytes[:]) // This function is documented to never fail.
	return fmt.Sprintf("%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		uuid_bytes[0], uuid_bytes[1], uuid_bytes[2], uuid_bytes[3],
		uuid_bytes[4], uuid_bytes[5],
		uuid_bytes[6], uuid_bytes[7],
		uuid_bytes[8], uuid_bytes[9],
		uuid_bytes[10], uuid_bytes[11], uuid_bytes[12], uuid_bytes[13], uuid_bytes[14], uuid_bytes[15])
}
