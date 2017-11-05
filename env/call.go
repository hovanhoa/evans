package env

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"google.golang.org/grpc"

	"github.com/AlecAivazis/survey"
	prompt "github.com/c-bata/go-prompt"
	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/protoc-gen-go/descriptor"
	"github.com/jhump/protoreflect/desc"
	"github.com/jhump/protoreflect/dynamic"
	"github.com/k0kubun/pp"
	"github.com/pkg/errors"
)

// fieldable is only used to set primitive, enum, oneof fields.
type fieldable interface {
	fieldable()
}

type baseField struct {
	name     string
	descType descriptor.FieldDescriptorProto_Type
	desc     *desc.FieldDescriptor
}

func (f *baseField) fieldable() {}

// primitiveField is used to read and store input for each primitiveField
type primitiveField struct {
	*baseField
	val string
}

type messageField struct {
	*baseField
	val []fieldable
}

type oneOfField struct {
	*baseField
}

// Call calls a RPC which is selected
// RPC is called after inputting field values interactively
func (e *Env) Call(name string) (string, error) {
	rpc, err := e.GetRPC(name)
	if err != nil {
		return "", err
	}

	// TODO: GetFields は OneOf の要素まで取得してしまう
	input, err := e.inputFields([]string{}, rpc.RequestType, prompt.DarkGreen)
	if errors.Cause(err) == io.EOF {
		return "", nil
	} else if err != nil {
		return "", err
	}

	req := dynamic.NewMessage(rpc.RequestType)
	if err = e.setInput(req, input); err != nil {
		return "", err
	}

	res := dynamic.NewMessage(rpc.ResponseType)
	conn, err := grpc.Dial(fmt.Sprintf("%s:%s", e.config.Server.Host, e.config.Server.Port), grpc.WithInsecure())
	if err != nil {
		return "", err
	}
	defer conn.Close()

	ep := e.genEndpoint(name)
	if err := grpc.Invoke(context.Background(), ep, req, res, conn); err != nil {
		return "", err
	}

	m := jsonpb.Marshaler{Indent: "  "}
	json, err := m.MarshalToString(res)
	if err != nil {
		return "", err
	}

	return json + "\n", nil
}

func (e *Env) genEndpoint(rpcName string) string {
	ep := fmt.Sprintf("/%s.%s/%s", e.state.currentPackage, e.state.currentService, rpcName)
	return ep
}

func (e *Env) setInput(req *dynamic.Message, fields []fieldable) error {
	for _, field := range fields {
		switch f := field.(type) {
		case *primitiveField:
			pv := f.val

			// it holds value and error of conversion
			// each cast (Parse*) returns falsy value when failed to parse argument
			var v interface{}
			var err error

			switch f.descType {
			case descriptor.FieldDescriptorProto_TYPE_DOUBLE:
				v, err = strconv.ParseFloat(pv, 64)

			case descriptor.FieldDescriptorProto_TYPE_FLOAT:
				v, err = strconv.ParseFloat(pv, 32)
				v = float32(v.(float64))

			case descriptor.FieldDescriptorProto_TYPE_INT64:
				v, err = strconv.ParseInt(pv, 10, 64)

			case descriptor.FieldDescriptorProto_TYPE_UINT64:
				v, err = strconv.ParseUint(pv, 10, 64)

			case descriptor.FieldDescriptorProto_TYPE_INT32:
				v, err = strconv.ParseInt(f.val, 10, 32)
				v = int32(v.(int64))

			case descriptor.FieldDescriptorProto_TYPE_UINT32:
				v, err = strconv.ParseUint(pv, 10, 32)
				v = uint32(v.(uint64))

			case descriptor.FieldDescriptorProto_TYPE_FIXED64:
				v, err = strconv.ParseUint(pv, 10, 64)

			case descriptor.FieldDescriptorProto_TYPE_FIXED32:
				v, err = strconv.ParseUint(pv, 10, 32)
				v = uint32(v.(uint64))

			case descriptor.FieldDescriptorProto_TYPE_BOOL:
				v, err = strconv.ParseBool(pv)

			case descriptor.FieldDescriptorProto_TYPE_STRING:
				// already string
				v = pv

			case descriptor.FieldDescriptorProto_TYPE_BYTES:
				v = []byte(pv)

			case descriptor.FieldDescriptorProto_TYPE_SFIXED64:
				v, err = strconv.ParseUint(pv, 10, 64)

			case descriptor.FieldDescriptorProto_TYPE_SFIXED32:
				v, err = strconv.ParseUint(pv, 10, 32)
				v = int32(v.(int64))

			case descriptor.FieldDescriptorProto_TYPE_SINT64:
				v, err = strconv.ParseInt(pv, 10, 64)

			case descriptor.FieldDescriptorProto_TYPE_SINT32:
				v, err = strconv.ParseInt(pv, 10, 32)
				v = int32(v.(int64))

			default:
				return fmt.Errorf("invalid type: %#v", f.descType)
			}

			if err != nil {
				return err
			}
			if err := req.TrySetField(f.desc, v); err != nil {
				return err
			}

		case *messageField:
			// TODO
			msg := dynamic.NewMessage(f.desc.GetMessageType())
			if err := e.setInput(msg, f.val); err != nil {
				return err
			}
			req.SetField(f.desc, msg)
		}
	}
	return nil
}

func (e *Env) inputFields(ancestor []string, msg *desc.MessageDescriptor, color prompt.Color) ([]fieldable, error) {
	fields := msg.GetFields()

	input := make([]fieldable, 0, len(fields))
	max := maxLen(fields, e.config.InputPromptFormat)
	promptFormat := fmt.Sprintf("%"+strconv.Itoa(max)+"s", e.config.InputPromptFormat)
	encounteredOneOf := map[string]bool{}
	encounteredEnum := map[string]bool{}
	for _, f := range fields {
		if oneOf := f.GetOneOf(); oneOf != nil {
			if encounteredOneOf[oneOf.GetFullyQualifiedName()] {
				continue
			}

			encounteredOneOf[oneOf.GetFullyQualifiedName()] = true

			opts := make([]string, len(oneOf.GetChoices()))
			optMap := map[string]*desc.FieldDescriptor{}
			for i, c := range oneOf.GetChoices() {
				opts[i] = c.GetName()
				optMap[c.GetName()] = c
			}

			var choice string
			err := survey.AskOne(&survey.Select{
				Message: oneOf.GetName(),
				Options: opts,
			}, &choice, nil)
			if err != nil {
				return nil, err
			}

			f = optMap[choice]
		} else if enum := f.GetEnumType(); enum != nil {
			if encounteredEnum[enum.GetFullyQualifiedName()] {
				continue
			}

			encounteredEnum[enum.GetFullyQualifiedName()] = true

			opts := make([]string, len(enum.GetValues()))
			optMap := map[string]*desc.EnumValueDescriptor{}
			for i, o := range enum.GetValues() {
				opts[i] = o.GetName()
				optMap[o.GetFullyQualifiedName()] = o
			}

			var choice string
			err := survey.AskOne(&survey.Select{
				Message: enum.GetName(),
				Options: opts,
			}, &choice, nil)
			if err != nil {
				return nil, err
			}

			pp.Println(choice)
			// TODO: enum は input がない
			// f = optMap[choice].GetNumber
		}

		var in fieldable
		base := &baseField{
			name:     f.GetName(),
			desc:     f,
			descType: f.GetType(),
		}

		// message field, enum field or primitive field
		if isMessageType(f.GetType()) {
			fields, err := e.inputFields(append(ancestor, f.GetName()), f.GetMessageType(), color)
			if err != nil {
				return nil, errors.Wrap(err, "failed to read inputs")
			}
			in = &messageField{
				baseField: base,
				val:       fields,
			}
			color = prompt.DarkGreen + (color+1)%16
		} else {
			ancestor := strings.Join(ancestor, e.config.AncestorDelimiter)
			if ancestor != "" {
				ancestor = "@" + ancestor
			}
			promptFormat = strings.Replace(promptFormat, "{ancestor}", ancestor, -1)
			promptFormat = strings.Replace(promptFormat, "{name}", f.GetName(), -1)
			promptFormat = strings.Replace(promptFormat, "{type}", f.GetType().String(), -1)

			l := prompt.Input(
				promptFormat,
				inputCompleter,
				prompt.OptionPrefixTextColor(color),
			)
			in = &primitiveField{
				baseField: base,
				val:       l,
			}
		}

		input = append(input, in)
	}
	return input, nil
}

// func fieldInputer(config *config.Env, ancestor []string, inputPrompt string, color prompt.Color) func(*desc.FieldDescriptor) *field {
// 	promptFormat := config.InputPromptFormat
//
// 	return func(f *desc.FieldDescriptor) *field {
// 		ancestor := strings.Join(ancestor, config.AncestorDelimiter)
// 		if ancestor != "" {
// 			ancestor = "@" + ancestor
// 		}
// 		promptFormat = strings.Replace(promptFormat, "{ancestor}", ancestor, -1)
// 		promptFormat = strings.Replace(promptFormat, "{name}", f.GetName(), -1)
// 		promptFormat = strings.Replace(promptFormat, "{type}", f.GetType().String(), -1)
//
// 		l := prompt.Input(
// 			inputPrompt,
// 			inputCompleter,
// 			prompt.OptionPrefixTextColor(color),
// 		)
// 		in.isPrimitive = true
// 		in.pVal = &l
// 	}
// }

func maxLen(fields []*desc.FieldDescriptor, format string) int {
	var max int
	for _, f := range fields {
		if isMessageType(f.GetType()) {
			continue
		}
		prompt := format
		elems := map[string]string{
			"name": f.GetName(),
			"type": f.GetType().String(),
		}
		for k, v := range elems {
			prompt = strings.Replace(prompt, "{"+k+"}", v, -1)
		}
		l := len(format)
		if l > max {
			max = l
		}
	}
	return max
}

func isMessageType(typeName descriptor.FieldDescriptorProto_Type) bool {
	return typeName == descriptor.FieldDescriptorProto_TYPE_MESSAGE
}

func inputCompleter(d prompt.Document) []prompt.Suggest {
	return nil
}
