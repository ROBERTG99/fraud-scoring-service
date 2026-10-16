package main

import (
	"context"
	"testing"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/ROBERTG99/fraud-scoring-service/internal/stream"
)

func fetchCommitted(ctx context.Context, t *testing.T, cl *kgo.Client, group string) map[int32]int64 {
	t.Helper()
	resp, err := kadm.NewClient(cl).FetchOffsets(ctx, group)
	if err != nil {
		return nil
	}
	out := map[int32]int64{}
	resp.Each(func(o kadm.OffsetResponse) {
		if o.Topic == stream.TopicLogin {
			out[o.Partition] = o.At
		}
	})
	return out
}
