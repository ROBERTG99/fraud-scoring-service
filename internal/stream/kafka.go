package stream

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
)

// Partitions per topic (spec FR-2).
const Partitions = 12

// AllTopics lists every topic the system uses.
var AllTopics = []string{TopicLogin, TopicDevice, TopicTransaction, TopicDecisions, TopicDLQ}

// Brokers splits a comma-separated broker list.
func Brokers(s string) []string {
	var out []string
	for _, b := range strings.Split(s, ",") {
		if b = strings.TrimSpace(b); b != "" {
			out = append(out, b)
		}
	}
	return out
}

// EnsureTopics creates missing topics with the given partition count and replication factor.
func EnsureTopics(ctx context.Context, cl *kgo.Client, partitions int32, replication int16, topics ...string) error {
	adm := kadm.NewClient(cl)
	resp, err := adm.CreateTopics(ctx, partitions, replication, nil, topics...)
	if err != nil {
		return fmt.Errorf("create topics: %w", err)
	}
	for _, r := range resp.Sorted() {
		if r.Err != nil && !errors.Is(r.Err, kerr.TopicAlreadyExists) {
			return fmt.Errorf("create topic %s: %w", r.Topic, r.Err)
		}
	}
	return nil
}
