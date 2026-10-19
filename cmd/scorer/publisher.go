package main

import (
	"context"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/ROBERTG99/fraud-scoring-service/internal/store"
	"github.com/ROBERTG99/fraud-scoring-service/internal/stream"
	"github.com/ROBERTG99/fraud-scoring-service/internal/telemetry"
)

// kafkaPublisher sends events.transaction without blocking the request: TryProduce fails immediately when
// the producer buffer is full, and the drop is counted (fraud_publish_dropped_total).
type kafkaPublisher struct{ cl *kgo.Client }

func (p kafkaPublisher) PublishTransaction(rec store.DecisionRecord) {
	if rec.Event == nil {
		return
	}
	r := &kgo.Record{Topic: stream.TopicTransaction, Key: []byte(rec.AccountID), Value: rec.Event.AppendJSON(nil)}
	p.cl.TryProduce(context.Background(), r, func(_ *kgo.Record, err error) {
		if err != nil {
			telemetry.PublishDropped.Inc()
		}
	})
}
