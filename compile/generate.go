// Copyright (c) 2026 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package compile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"go.uber.org/thriftrw/ast"
)

// GenerateThriftFile writes the Thrift IDL representation of this Module to the
// given path. The content is reconstructed entirely from the Module's compiled
// structured fields (namespaces, includes, constants, types, services) and does
// not rely on the raw IDL input.
//
// GenerateThriftFile is, in effect, the inverse of Compile: compiling the
// generated file yields a Module structurally equivalent to the receiver.
func (m *Module) GenerateThriftFile(path string) error {
	if m == nil {
		return fmt.Errorf("nil module")
	}

	content, err := m.thriftIDL()
	if err != nil {
		return fmt.Errorf("generate thrift content for %q: %w", path, err)
	}

	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create directory for %q: %w", path, err)
		}
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write thrift file %q: %w", path, err)
	}

	return nil
}

// thriftIDL builds the Thrift IDL representation of the Module as a string.
// Sections are emitted in the order: namespaces, includes, constants, types,
// services.
func (m *Module) thriftIDL() (string, error) {
	var builder strings.Builder

	writeNamespaces(&builder, m)

	if err := writeIncludes(&builder, m); err != nil {
		return "", err
	}

	if err := writeConstants(&builder, m); err != nil {
		return "", err
	}

	if err := writeTypes(&builder, m); err != nil {
		return "", err
	}

	if err := writeServices(&builder, m); err != nil {
		return "", err
	}

	return builder.String(), nil
}

// sortedKeys returns the keys of m sorted lexicographically. It is used to give
// the generated output a deterministic ordering for map-backed declarations.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// writeNamespaces writes the namespace declarations stored on the module.
// Namespaces are emitted in sorted scope order for deterministic output.
func writeNamespaces(builder *strings.Builder, module *Module) {
	if len(module.Namespaces) == 0 {
		return
	}

	for _, scope := range sortedKeys(module.Namespaces) {
		builder.WriteString(fmt.Sprintf("namespace %s %s\n", scope, module.Namespaces[scope]))
	}
	builder.WriteString("\n")
}

// writeIncludes writes the include statements for the module, reconstructing
// each include path relative to the directory of the current thrift file.
func writeIncludes(builder *strings.Builder, module *Module) error {
	if len(module.Includes) == 0 {
		return nil
	}

	moduleDir := filepath.Dir(module.ThriftPath)

	for _, name := range sortedKeys(module.Includes) {
		includedModule, ok := module.Includes[name]
		if !ok || includedModule == nil || includedModule.Module == nil {
			return fmt.Errorf("included module %q is nil in thrift file: %s", name, module.ThriftPath)
		}

		relPath, err := filepath.Rel(moduleDir, includedModule.Module.ThriftPath)
		if err != nil {
			return fmt.Errorf("error while computing relative include path for %q in thrift file %s: %w", name, module.ThriftPath, err)
		}

		builder.WriteString(fmt.Sprintf("include \"%s\"\n", relPath))
	}
	builder.WriteString("\n")

	return nil
}

// writeConstants writes constant definitions to the builder using the compiled
// constant values, in sorted name order for deterministic output.
func writeConstants(builder *strings.Builder, module *Module) error {
	if len(module.Constants) == 0 {
		return nil
	}

	for _, name := range sortedKeys(module.Constants) {
		constant, ok := module.Constants[name]
		if !ok || constant == nil {
			continue
		}

		writeDocumentation(builder, constant.Doc)

		typeStr, err := getType(constant.Type, module)
		if err != nil {
			return fmt.Errorf("error while getting type for constant %s in thrift file %s: %w", name, module.ThriftPath, err)
		}

		valueStr, err := constantValueToString(constant.Value, module, 0)
		if err != nil {
			return fmt.Errorf("error while getting value for constant %s in thrift file %s: %w", name, module.ThriftPath, err)
		}

		builder.WriteString(fmt.Sprintf("const %s %s = %s\n", typeStr, name, valueStr))
	}

	builder.WriteString("\n")
	return nil
}

// constantValueToString serializes a compiled ConstantValue back into its
// Thrift literal representation. indent is the current indentation depth (in
// levels of two spaces) used for multi-line container literals.
func constantValueToString(value ConstantValue, module *Module, indent int) (string, error) {
	switch v := value.(type) {
	case ConstantBool:
		if bool(v) {
			return "true", nil
		}
		return "false", nil
	case ConstantInt:
		return strconv.FormatInt(int64(v), 10), nil
	case ConstantString:
		return fmt.Sprintf("\"%s\"", escapeString(string(v))), nil
	case ConstantDouble:
		return strconv.FormatFloat(float64(v), 'g', -1, 64), nil
	case ConstantList:
		return constantSliceToString([]ConstantValue(v), module, indent)
	case ConstantSet:
		return constantSliceToString([]ConstantValue(v), module, indent)
	case ConstantMap:
		return constantMapToString(v, module, indent)
	case *ConstantStruct:
		return constantStructToString(v, module, indent)
	case ConstReference:
		if v.Target == nil {
			return "", fmt.Errorf("constant reference has nil target in thrift file: %s", module.ThriftPath)
		}
		return getQualifiedTypeName(v.Target.Name, v.Target.File, module)
	case EnumItemReference:
		if v.Enum == nil || v.Item == nil {
			return "", fmt.Errorf("enum item reference is incomplete in thrift file: %s", module.ThriftPath)
		}
		enumName, err := getQualifiedTypeName(v.Enum.Name, v.Enum.File, module)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s.%s", enumName, v.Item.Name), nil
	}

	return "", fmt.Errorf("unknown constant value type %T in thrift file: %s", value, module.ThriftPath)
}

// constantSliceToString renders a list or set constant literal.
func constantSliceToString(items []ConstantValue, module *Module, indent int) (string, error) {
	if len(items) == 0 {
		return "[]", nil
	}

	itemIndent := strings.Repeat("  ", indent+1)
	closeIndent := strings.Repeat("  ", indent)

	var builder strings.Builder
	builder.WriteString("[\n")
	for _, item := range items {
		itemStr, err := constantValueToString(item, module, indent+1)
		if err != nil {
			return "", err
		}
		builder.WriteString(itemIndent + itemStr + ",\n")
	}
	builder.WriteString(closeIndent + "]")

	return builder.String(), nil
}

// constantMapToString renders a map constant literal.
func constantMapToString(items ConstantMap, module *Module, indent int) (string, error) {
	if len(items) == 0 {
		return "{}", nil
	}

	itemIndent := strings.Repeat("  ", indent+1)
	closeIndent := strings.Repeat("  ", indent)

	var builder strings.Builder
	builder.WriteString("{\n")
	for _, pair := range items {
		keyStr, err := constantValueToString(pair.Key, module, indent+1)
		if err != nil {
			return "", err
		}
		valueStr, err := constantValueToString(pair.Value, module, indent+1)
		if err != nil {
			return "", err
		}
		builder.WriteString(itemIndent + keyStr + ": " + valueStr + ",\n")
	}
	builder.WriteString(closeIndent + "}")

	return builder.String(), nil
}

// constantStructToString renders a struct constant literal (a map with string keys).
func constantStructToString(structValue *ConstantStruct, module *Module, indent int) (string, error) {
	if structValue == nil || len(structValue.Fields) == 0 {
		return "{}", nil
	}

	itemIndent := strings.Repeat("  ", indent+1)
	closeIndent := strings.Repeat("  ", indent)

	var builder strings.Builder
	builder.WriteString("{\n")
	for _, fieldName := range sortedKeys(structValue.Fields) {
		valueStr, err := constantValueToString(structValue.Fields[fieldName], module, indent+1)
		if err != nil {
			return "", err
		}
		builder.WriteString(fmt.Sprintf("%s\"%s\": %s,\n", itemIndent, fieldName, valueStr))
	}
	builder.WriteString(closeIndent + "}")

	return builder.String(), nil
}

// writeTypes writes the typedef, enum, struct, union, and exception definitions.
func writeTypes(builder *strings.Builder, module *Module) error {
	if len(module.Types) == 0 {
		return nil
	}

	// Sort types with enums first, exceptions last and other typeSpecs in between.
	for _, name := range sortTypesByKind(module) {
		typeSpec := module.Types[name]

		switch t := typeSpec.(type) {
		case *TypedefSpec:
			if err := writeTypedef(builder, t, module); err != nil {
				return fmt.Errorf("error while writing typedef: %w", err)
			}
		case *EnumSpec:
			if err := writeEnum(builder, t); err != nil {
				return fmt.Errorf("error while writing enum: %w", err)
			}
		case *StructSpec:
			// StructSpec includes structs, unions, and exceptions.
			if err := writeStruct(builder, t, module); err != nil {
				return fmt.Errorf("error while writing struct: %w", err)
			}
		}
		builder.WriteString("\n")
	}

	return nil
}

// writeDocumentation writes a documentation comment block, if doc is non-empty.
func writeDocumentation(builder *strings.Builder, doc string) {
	if doc == "" {
		return
	}

	lines := strings.Split(strings.TrimSpace(doc), "\n")
	builder.WriteString("/**")
	builder.WriteString("\n")
	for _, line := range lines {
		builder.WriteString("* " + line + "\n")
	}
	builder.WriteString("*/")
	builder.WriteString("\n")
}

// sortTypesByKind returns the names of the module's types ordered so that the
// generated output declares them in a stable, dependency-friendly order:
//
//  1. Enums
//  2. Typedefs
//  3. Structs
//  4. Unions
//  5. Exceptions
//
// Within each kind, names are sorted alphabetically.
func sortTypesByKind(module *Module) []string {
	typeNames := make([]string, 0, len(module.Types))
	for name := range module.Types {
		typeNames = append(typeNames, name)
	}

	sort.Slice(typeNames, func(i, j int) bool {
		priorityI := getTypePriority(module.Types[typeNames[i]])
		priorityJ := getTypePriority(module.Types[typeNames[j]])

		if priorityI != priorityJ {
			return priorityI < priorityJ
		}
		return typeNames[i] < typeNames[j]
	})

	return typeNames
}

// getTypePriority returns the sorting priority for a type. Lower numbers come
// first in the sorted output.
func getTypePriority(typeSpec TypeSpec) int {
	switch t := typeSpec.(type) {
	case *EnumSpec:
		return 1
	case *TypedefSpec:
		return 2
	case *StructSpec:
		switch t.Type {
		case ast.StructType:
			return 3
		case ast.UnionType:
			return 4
		case ast.ExceptionType:
			return 6
		}
	}
	// Unreachable for the current set of type specs; kept to accommodate any
	// new TypeSpec added in the future.
	return 5
}

// writeTypedef writes a typedef definition.
func writeTypedef(builder *strings.Builder, typedef *TypedefSpec, module *Module) error {
	if typedef == nil {
		return fmt.Errorf("typedef spec is nil")
	}

	writeDocumentation(builder, typedef.Doc)

	targetType, err := getType(typedef.Target, module)
	if err != nil {
		return fmt.Errorf("error while getting type name for typedef: %w", err)
	}
	builder.WriteString(fmt.Sprintf("typedef %s %s", targetType, typedef.Name))

	if annotations := getAnnotations(typedef.Annotations); annotations != "" {
		builder.WriteString(" " + annotations)
	}

	builder.WriteString("\n")
	return nil
}

// getType returns the Thrift type name of the given typeSpec. References to
// named types defined in included modules are qualified with the include alias.
func getType(typeSpec TypeSpec, module *Module) (string, error) {
	if typeSpec == nil {
		return "", fmt.Errorf("typeSpec is nil")
	}

	switch t := typeSpec.(type) {
	case *BoolSpec:
		return "bool", nil
	case *I8Spec:
		return "i8", nil
	case *I16Spec:
		return "i16", nil
	case *I32Spec:
		return "i32", nil
	case *I64Spec:
		return "i64", nil
	case *DoubleSpec:
		return "double", nil
	case *StringSpec:
		return "string", nil
	case *BinarySpec:
		return "binary", nil
	case *ListSpec:
		typeName, err := getType(t.ValueSpec, module)
		if err != nil {
			return "", fmt.Errorf("error while getting type string for list %s : %w", t.ValueSpec.ThriftName(), err)
		}
		return fmt.Sprintf("list<%s>", typeName), nil

	case *SetSpec:
		typeName, err := getType(t.ValueSpec, module)
		if err != nil {
			return "", fmt.Errorf("error while getting type string for set %s : %w", t.ValueSpec.ThriftName(), err)
		}
		return fmt.Sprintf("set<%s>", typeName), nil

	case *MapSpec:
		keyType, err := getType(t.KeySpec, module)
		if err != nil {
			return "", fmt.Errorf("error while getting type string for map key %s : %w", t.KeySpec.ThriftName(), err)
		}
		valueType, err := getType(t.ValueSpec, module)
		if err != nil {
			return "", fmt.Errorf("error while getting type string for map value %s : %w", t.ValueSpec.ThriftName(), err)
		}
		return fmt.Sprintf("map<%s, %s>", keyType, valueType), nil

	case *TypedefSpec:
		return getQualifiedTypeName(t.Name, t.File, module)

	case *EnumSpec:
		return getQualifiedTypeName(t.Name, t.File, module)

	case *StructSpec:
		return getQualifiedTypeName(t.Name, t.File, module)
	}

	return "", fmt.Errorf("unknown type: %T in thrift file: %s", typeSpec, module.ThriftPath)
}

// getAnnotations converts annotations to their Thrift string representation,
// e.g. (go.name = "Foo", deprecated). Annotations are sorted by name.
func getAnnotations(annotations Annotations) string {
	if len(annotations) == 0 {
		return ""
	}

	parts := make([]string, 0, len(annotations))
	for _, name := range sortedKeys(annotations) {
		value := annotations[name]
		if value != "" {
			parts = append(parts, fmt.Sprintf("%s = \"%s\"", name, escapeString(value)))
		} else {
			parts = append(parts, name)
		}
	}

	return fmt.Sprintf("(%s)", strings.Join(parts, ", "))
}

// getQualifiedTypeName returns the qualified name for a type, prefixing it with
// the include alias if the type is defined in an included module.
func getQualifiedTypeName(typeName, typeFilePath string, module *Module) (string, error) {
	// If the type's file is the same as the current module's file, it's local.
	if typeFilePath == module.ThriftPath {
		return typeName, nil
	}

	// Check if this type comes from an included module.
	for _, includedModule := range module.Includes {
		if includedModule != nil && includedModule.Module != nil && typeFilePath == includedModule.Module.ThriftPath {
			return fmt.Sprintf("%s.%s", includedModule.Name, typeName), nil
		}
	}

	return "", fmt.Errorf("unable to resolve qualified type name for type: %s from file: %s", typeName, typeFilePath)
}

// escapeString escapes special characters in a Thrift string literal.
func escapeString(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	s = strings.ReplaceAll(s, "\t", "\\t")
	return s
}

// writeEnum writes an enum definition.
func writeEnum(builder *strings.Builder, enum *EnumSpec) error {
	if enum == nil {
		return fmt.Errorf("enum spec is nil")
	}

	writeDocumentation(builder, enum.Doc)

	builder.WriteString(fmt.Sprintf("enum %s {\n", enum.Name))

	for i, item := range enum.Items {
		writeDocumentation(builder, item.Doc)

		builder.WriteString(fmt.Sprintf("  %s = %d", item.Name, item.Value))

		if annotations := getAnnotations(item.Annotations); annotations != "" {
			builder.WriteString(" " + annotations)
		}

		if i < len(enum.Items)-1 {
			builder.WriteString(",")
		}
		builder.WriteString("\n")
	}

	builder.WriteString("}")

	if annotations := getAnnotations(enum.Annotations); annotations != "" {
		builder.WriteString(" " + annotations)
	}

	builder.WriteString("\n")
	return nil
}

// writeStruct writes a struct, union, or exception definition.
func writeStruct(builder *strings.Builder, structSpec *StructSpec, module *Module) error {
	if structSpec == nil {
		return fmt.Errorf("struct spec is nil")
	}

	writeDocumentation(builder, structSpec.Doc)

	var keyword string
	switch structSpec.Type {
	case ast.StructType:
		keyword = "struct"
	case ast.UnionType:
		keyword = "union"
	case ast.ExceptionType:
		keyword = "exception"
	default:
		return fmt.Errorf("unknown structSpec type %s in thrift file: %s", structSpec.Name, module.ThriftPath)
	}

	builder.WriteString(fmt.Sprintf("%s %s {\n", keyword, structSpec.Name))

	for _, field := range structSpec.Fields {
		writeDocumentation(builder, field.Doc)

		builder.WriteString(fmt.Sprintf("  %d: ", field.ID))

		// Requiredness. Unions never carry a requiredness keyword.
		if field.Required {
			builder.WriteString("required ")
		} else if keyword != "union" {
			builder.WriteString("optional ")
		}

		fieldType, err := getType(field.Type, module)
		if err != nil {
			return fmt.Errorf("error while getting type string for field %s : %w in thrift file: %s", field.Name, err, module.ThriftPath)
		}
		builder.WriteString(fieldType + " ")

		// Annotations carried by the field's (scalar) type spec.
		if isScalarField(field) {
			if typeSpecAnnotations := getAnnotations(field.Type.ThriftAnnotations()); typeSpecAnnotations != "" {
				builder.WriteString(typeSpecAnnotations + " ")
			}
		}

		builder.WriteString(field.Name)

		if field.Default != nil {
			defaultValue, err := constantValueToString(field.Default, module, 0)
			if err != nil {
				return fmt.Errorf("error while getting default value for field %s in thrift file: %s : %w", field.Name, module.ThriftPath, err)
			}
			builder.WriteString(" = " + defaultValue)
		}

		if annotations := getAnnotations(field.Annotations); annotations != "" {
			builder.WriteString(" " + annotations)
		}

		builder.WriteString("\n")
	}

	builder.WriteString("}")

	if annotations := getAnnotations(structSpec.Annotations); annotations != "" {
		builder.WriteString(" " + annotations)
	}

	builder.WriteString("\n")
	return nil
}

// isScalarField reports whether the field's type is a primitive Thrift type.
func isScalarField(field *FieldSpec) bool {
	switch field.Type.(type) {
	case *BoolSpec, *I8Spec, *I16Spec, *I32Spec, *I64Spec, *DoubleSpec, *StringSpec, *BinarySpec:
		return true
	}
	return false
}

// writeServices writes service definitions, including their functions,
// arguments, return types, exceptions, and inheritance.
func writeServices(builder *strings.Builder, module *Module) error {
	if len(module.Services) == 0 {
		return nil
	}

	for _, serviceName := range sortedKeys(module.Services) {
		service := module.Services[serviceName]

		builder.WriteString(fmt.Sprintf("service %s", serviceName))

		if service.Parent != nil {
			parentName, err := getOriginalServiceParentName(module, serviceName, service.Parent)
			if err != nil {
				return fmt.Errorf("error while getting parent service name for service %s in thrift file: %s : %w", serviceName, module.ThriftPath, err)
			}
			builder.WriteString(fmt.Sprintf(" extends %s", parentName))
		}

		builder.WriteString(" {\n")

		for _, fname := range sortedKeys(service.Functions) {
			function := service.Functions[fname]

			if function.OneWay {
				builder.WriteString("  oneway ")
			} else {
				builder.WriteString("  ")
			}

			if function.ResultSpec != nil && function.ResultSpec.ReturnType != nil {
				returnType, err := getType(function.ResultSpec.ReturnType, module)
				if err != nil {
					return fmt.Errorf("error while getting return type %s : %w in thrift file: %s", function.ResultSpec.ReturnType.ThriftName(), err, module.ThriftPath)
				}
				builder.WriteString(returnType + " ")
			} else {
				builder.WriteString("void ")
			}

			builder.WriteString(fname + "(")

			for i, arg := range FieldGroup(function.ArgsSpec) {
				if i > 0 {
					builder.WriteString(", ")
				}

				fieldType, err := getType(arg.Type, module)
				if err != nil {
					return fmt.Errorf("error while getting data type for function argument %s for function %s in thrift file: %s : %w", arg.Name, function.Name, module.ThriftPath, err)
				}
				if arg.Required {
					builder.WriteString(fmt.Sprintf("%d: required %s %s", arg.ID, fieldType, arg.Name))
				} else {
					builder.WriteString(fmt.Sprintf("%d: %s %s", arg.ID, fieldType, arg.Name))
				}
			}

			builder.WriteString(")")

			if function.ResultSpec != nil && len(function.ResultSpec.Exceptions) > 0 {
				builder.WriteString(" throws (")
				for i, exc := range function.ResultSpec.Exceptions {
					if i > 0 {
						builder.WriteString(", ")
					}

					fieldType, err := getType(exc.Type, module)
					if err != nil {
						return fmt.Errorf("error while getting data type for exception %s in thrift file: %s : %w", exc.Name, module.ThriftPath, err)
					}
					if exc.Required {
						builder.WriteString(fmt.Sprintf("%d: required %s %s", exc.ID, fieldType, exc.Name))
					} else {
						builder.WriteString(fmt.Sprintf("%d: %s %s", exc.ID, fieldType, exc.Name))
					}
				}
				builder.WriteString(")")
			}

			if annotations := getAnnotations(function.Annotations); annotations != "" {
				builder.WriteString(" " + annotations)
			}

			builder.WriteString("\n")
		}

		builder.WriteString("}")

		if annotations := getAnnotations(service.Annotations); annotations != "" {
			builder.WriteString(" " + annotations)
		}

		builder.WriteString("\n\n")
	}

	return nil
}

// getOriginalServiceParentName resolves the name of a service's parent,
// qualifying it with the include alias when the parent is defined in an
// included module.
func getOriginalServiceParentName(module *Module, serviceName string, parent *ServiceSpec) (string, error) {
	if parent == nil {
		return "", fmt.Errorf("parent service spec is nil")
	}

	if parent.File == module.ThriftPath {
		return parent.Name, nil
	}

	for includeName, include := range module.Includes {
		if include != nil && include.Module != nil && parent.File == include.Module.ThriftPath {
			return fmt.Sprintf("%s.%s", includeName, parent.Name), nil
		}
	}

	return "", fmt.Errorf("could not find original service parent name for %s", serviceName)
}
