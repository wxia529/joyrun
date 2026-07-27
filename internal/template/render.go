package template

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	texttemplate "text/template"
	"text/template/parse"

	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/model"
)

func Validate(value string, params map[string]model.ParamSpec) error {
	tmpl, err := texttemplate.New("validate").Parse(value)
	if err != nil {
		return fault.Wrap("TARGET_INVALID", "cannot parse template", false, err)
	}
	if tmpl.Tree == nil || tmpl.Tree.Root == nil {
		return nil
	}
	for _, node := range tmpl.Tree.Root.Nodes {
		switch typed := node.(type) {
		case *parse.TextNode:
			continue
		case *parse.ActionNode:
			if err := validateAction(typed, params); err != nil {
				return err
			}
		default:
			return fault.New("TARGET_INVALID", "templates only support direct variable substitutions; control structures are not allowed", false)
		}
	}
	return nil
}

func validateAction(action *parse.ActionNode, params map[string]model.ParamSpec) error {
	if action.Pipe == nil || len(action.Pipe.Decl) != 0 || len(action.Pipe.Cmds) != 1 {
		return fault.New("TARGET_INVALID", "template pipelines and declarations are not allowed", false)
	}
	command := action.Pipe.Cmds[0]
	if len(command.Args) != 1 {
		return fault.New("TARGET_INVALID", "template actions must contain one variable", false)
	}
	field, ok := command.Args[0].(*parse.FieldNode)
	if !ok {
		return fault.New("TARGET_INVALID", "template actions must be direct variable substitutions", false)
	}
	if len(field.Ident) == 1 {
		switch field.Ident[0] {
		case "Input", "Stem", "Name", "TaskID", "WorkDir":
			return nil
		}
	}
	if len(field.Ident) == 2 && field.Ident[0] == "Params" {
		if _, ok := params[field.Ident[1]]; ok {
			return nil
		}
		return fault.New("TARGET_INVALID", "template refers to undeclared parameter "+field.Ident[1], false)
	}
	return fault.New("TARGET_INVALID", "template refers to an unsupported variable", false)
}

type Data struct {
	Input   string
	Stem    string
	Name    string
	TaskID  string
	WorkDir string
	Params  map[string]any
}

func Values(source model.Source, taskID, remoteWorkDir, sourceName string, params map[string]any) Data {
	input, stem := "", ""
	if source.Entry != nil {
		input = *source.Entry
		stem = input[:len(input)-len(filepath.Ext(input))]
	}
	return Data{Input: input, Stem: stem, Name: sourceName, TaskID: taskID, WorkDir: remoteWorkDir, Params: params}
}

func Render(target model.Target, data Data) (string, error) {
	tmpl, err := texttemplate.New("job").Option("missingkey=error").Parse(target.Script)
	if err != nil {
		return "", fault.Wrap("TARGET_INVALID", "cannot parse target script template", false, err)
	}
	safeData, err := shellData(data)
	if err != nil {
		return "", err
	}
	var output bytes.Buffer
	if err := tmpl.Execute(&output, safeData); err != nil {
		return "", fault.Wrap("TARGET_INVALID", "cannot render target script template", false, err)
	}
	return output.String(), nil
}

func shellData(data Data) (Data, error) {
	result := data
	for name, value := range map[string]string{
		"Input": data.Input, "Stem": data.Stem, "Name": data.Name,
		"TaskID": data.TaskID, "WorkDir": data.WorkDir,
	} {
		if hasLineBreak(value) {
			return Data{}, fault.New("UNSAFE_TEMPLATE_VALUE", name+" contains a line break", false)
		}
	}
	result.Input = shellQuote(data.Input)
	result.Stem = shellQuote(data.Stem)
	result.Name = shellQuote(data.Name)
	result.TaskID = shellQuote(data.TaskID)
	result.WorkDir = shellQuote(data.WorkDir)
	result.Params = make(map[string]any, len(data.Params))
	for key, value := range data.Params {
		if text, ok := value.(string); ok {
			if hasLineBreak(text) {
				return Data{}, fault.New("UNSAFE_TEMPLATE_VALUE", "parameter "+key+" contains a line break", false)
			}
			result.Params[key] = shellQuote(text)
		} else {
			result.Params[key] = value
		}
	}
	return result, nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(fmt.Sprint(value), "'", "'\"'\"'") + "'"
}

func hasLineBreak(value string) bool {
	return strings.ContainsAny(value, "\r\n\x00")
}

func RenderString(value string, data Data) (string, error) {
	tmpl, err := texttemplate.New("value").Option("missingkey=error").Parse(value)
	if err != nil {
		return "", err
	}
	var output bytes.Buffer
	if err := tmpl.Execute(&output, data); err != nil {
		return "", err
	}
	return output.String(), nil
}

func TargetHash(target model.Target) string {
	data, _ := json.Marshal(target)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
