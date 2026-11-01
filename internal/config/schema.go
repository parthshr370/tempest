package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// settingsSchema is the type every settings layer decodes into.
var settingsSchema = reflect.TypeFor[fileSettings]()

var rawMessageType = reflect.TypeFor[json.RawMessage]()

// pruneUnknownSettings removes, at any depth, every object member the
// settings schema does not declare under exactly that name, and returns the
// removed paths sorted. encoding/json matches member names case-insensitively
// (including Unicode folds such as U+017F for s), so a member that trust
// gates and requirement checks compare by exact name must never reach it in
// another spelling. Members of maps keep any name: they are data, such as
// provider routes, header names or environment variables.
func pruneUnknownSettings(members map[string]*settingsNode) []string {
	var removed []string
	pruneObject(members, settingsSchema, "", &removed)
	slices.Sort(removed)
	return removed
}

func pruneObject(members map[string]*settingsNode, schema reflect.Type, path string, removed *[]string) {
	fields := schemaFields(schema)
	for name, member := range members {
		memberPath := joinSettingPath(path, name)
		memberSchema, declared := fields[name]
		if !declared {
			delete(members, name)
			*removed = append(*removed, memberPath)
			continue
		}
		pruneValue(member, memberSchema, memberPath, removed)
	}
}

// pruneValue walks node against schema. A node whose shape does not match
// is left alone: decoding reports it as a wrong type.
func pruneValue(node *settingsNode, schema reflect.Type, path string, removed *[]string) {
	for schema.Kind() == reflect.Pointer {
		schema = schema.Elem()
	}
	switch {
	case schema == rawMessageType || schema.Kind() == reflect.Interface:
		// Free-form JSON, such as a debug adapter's launch arguments.
	case schema.Kind() == reflect.Struct && node.object != nil:
		pruneObject(node.object, schema, path, removed)
	case schema.Kind() == reflect.Map && node.object != nil:
		for name, member := range node.object {
			pruneValue(member, schema.Elem(), joinSettingPath(path, name), removed)
		}
	case (schema.Kind() == reflect.Slice || schema.Kind() == reflect.Array) && node.isArray:
		for index, item := range node.array {
			pruneValue(item, schema.Elem(), fmt.Sprintf("%s[%d]", path, index), removed)
		}
	}
}

// schemaFields maps each member name a struct type decodes to that member's
// type, named as encoding/json names them: the json tag, else the Go field
// name, with the exported fields of untagged embedded structs promoted
// unless an outer field has the same name.
func schemaFields(schema reflect.Type) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	var embedded []reflect.Type
	for index := range schema.NumField() {
		field := schema.Field(index)
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if field.Anonymous && name == "" {
			embedded = append(embedded, field.Type)
			continue
		}
		if !field.IsExported() {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = field.Type
	}
	for _, inner := range embedded {
		for inner.Kind() == reflect.Pointer {
			inner = inner.Elem()
		}
		if inner.Kind() != reflect.Struct {
			continue
		}
		for name, fieldType := range schemaFields(inner) {
			if _, shadowed := fields[name]; !shadowed {
				fields[name] = fieldType
			}
		}
	}
	return fields
}

func joinSettingPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}
