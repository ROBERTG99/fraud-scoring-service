// Package stream holds Kafka topics, JSON Schema validation, producers and consumers.
package stream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ROBERTG99/fraud-scoring-service/schemas"
)

// Topic names.
const (
	TopicLogin       = "events.login"
	TopicDevice      = "events.device"
	TopicTransaction = "events.transaction"
	TopicDecisions   = "decisions"
	TopicDLQ         = "events.dlq"
)

// Schema names carried in each message's "schema" field.
const (
	SchemaLogin       = "fraud.event.login.v1"
	SchemaDevice      = "fraud.event.device.v1"
	SchemaTransaction = "fraud.event.transaction.v1"
	SchemaDecision    = "fraud.decision.v1"
	SchemaDLQ         = "fraud.dlq.v1"
)

// TopicSchema maps each topic to the schema its messages must declare.
var TopicSchema = map[string]string{
	TopicLogin:       SchemaLogin,
	TopicDevice:      SchemaDevice,
	TopicTransaction: SchemaTransaction,
	TopicDecisions:   SchemaDecision,
	TopicDLQ:         SchemaDLQ,
}

var schemaFiles = map[string]string{
	SchemaLogin:       "events/login.json",
	SchemaDevice:      "events/device.json",
	SchemaTransaction: "events/transaction.json",
	SchemaDecision:    "events/decision.json",
	SchemaDLQ:         "events/dlq.json",
}

// ErrInvalidEvent wraps every validation failure.
var ErrInvalidEvent = errors.New("invalid event")

// Validator validates event JSON against the shared schemas. Safe for concurrent use.
type Validator struct {
	byName map[string]*jsonschema.Schema
}

// NewValidator compiles the embedded schemas.
func NewValidator() (*Validator, error) {
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	v := &Validator{byName: make(map[string]*jsonschema.Schema, len(schemaFiles))}
	for name, file := range schemaFiles {
		raw, err := fs.ReadFile(schemas.FS, file)
		if err != nil {
			return nil, err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		url := "mem://" + file
		if err := c.AddResource(url, doc); err != nil {
			return nil, err
		}
		s, err := c.Compile(url)
		if err != nil {
			return nil, fmt.Errorf("compile %s: %w", file, err)
		}
		v.byName[name] = s
	}
	return v, nil
}

var (
	defaultOnce sync.Once
	defaultV    *Validator
	defaultErr  error
)

// MustValidator returns a process-wide validator and panics if the embedded schemas are broken.
func MustValidator() *Validator {
	defaultOnce.Do(func() { defaultV, defaultErr = NewValidator() })
	if defaultErr != nil {
		panic(defaultErr)
	}
	return defaultV
}

// Validate checks raw JSON against the schema named in its "schema" field and returns that name.
func (v *Validator) Validate(raw []byte) (string, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("%w: malformed JSON: %v", ErrInvalidEvent, err)
	}
	obj, ok := doc.(map[string]any)
	if !ok {
		return "", fmt.Errorf("%w: not a JSON object", ErrInvalidEvent)
	}
	name, _ := obj["schema"].(string)
	s, ok := v.byName[name]
	if !ok {
		return name, fmt.Errorf("%w: unknown schema %q", ErrInvalidEvent, name)
	}
	if err := s.Validate(doc); err != nil {
		return name, fmt.Errorf("%w: %s", ErrInvalidEvent, flatten(err))
	}
	return name, nil
}

// ValidateTopic validates raw and checks that its schema belongs to topic.
func (v *Validator) ValidateTopic(topic string, raw []byte) error {
	name, err := v.Validate(raw)
	if err != nil {
		return err
	}
	if want := TopicSchema[topic]; want != name {
		return fmt.Errorf("%w: topic %s expects schema %s, got %s", ErrInvalidEvent, topic, want, name)
	}
	return nil
}

func flatten(err error) string {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return err.Error()
	}
	out := ve.DetailedOutput()
	msgs := collect(out, nil)
	if len(msgs) == 0 {
		return ve.Error()
	}
	b, _ := json.Marshal(msgs)
	return string(b)
}

func collect(u *jsonschema.OutputUnit, acc []string) []string {
	if u.Error != nil {
		acc = append(acc, fmt.Sprintf("%s: %s", u.InstanceLocation, u.Error.String()))
	}
	for i := range u.Errors {
		acc = collect(&u.Errors[i], acc)
	}
	return acc
}
