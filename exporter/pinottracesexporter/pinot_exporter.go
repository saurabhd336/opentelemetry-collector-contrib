// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pinottracesexporter

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.opentelemetry.io/collector/config"
	"go.opentelemetry.io/collector/model/pdata"
	conventions "go.opentelemetry.io/collector/model/semconv/v1.5.0"
	"go.uber.org/zap"
)

// Crete new exporter.
func newExporter(cfg config.Exporter, logger *zap.Logger) (*storage, error) {

	pinotConfig := cfg.(*Config)
	storage := storage{pinotControllerUrl: pinotConfig.Datasource, kafkaUrl: pinotConfig.KafkaUrl}
	storage.init()

	return &storage, nil
}

func (s *storage) init() {
	// 1) Create schemas
	// 2) Create tables
	// 3) Initialize kafka client
	// 4) Create kafka topics

	dialer := &kafka.Dialer{
		Timeout: 10 * time.Second,
	}

	s.spanKafkaWriter = kafka.NewWriter(kafka.WriterConfig{
		Brokers:      []string{s.kafkaUrl},
		Topic:        "signoz-spans-topic",
		Dialer:       dialer,
		WriteTimeout: 10 * time.Second,
		ReadTimeout:  10 * time.Second,
	})

	s.indexKafkaWriter = kafka.NewWriter(kafka.WriterConfig{
		Brokers:      []string{s.kafkaUrl},
		Topic:        "signoz-index-v2-topic",
		Dialer:       dialer,
		WriteTimeout: 10 * time.Second,
		ReadTimeout:  10 * time.Second,
	})

	s.errorKafkaWriter = kafka.NewWriter(kafka.WriterConfig{
		Brokers:      []string{s.kafkaUrl},
		Topic:        "signoz-error-index-v2-topic",
		Dialer:       dialer,
		WriteTimeout: 10 * time.Second,
		ReadTimeout:  10 * time.Second,
	})
}

type storage struct {
	pinotControllerUrl string
	kafkaUrl           string
	spanKafkaWriter    *kafka.Writer
	indexKafkaWriter   *kafka.Writer
	errorKafkaWriter   *kafka.Writer
}

func makeJaegerProtoReferences(
	links pdata.SpanLinkSlice,
	parentSpanID pdata.SpanID,
	traceID pdata.TraceID,
) ([]OtelSpanRef, error) {

	parentSpanIDSet := len(parentSpanID.Bytes()) != 0
	if !parentSpanIDSet && links.Len() == 0 {
		return nil, nil
	}

	refsCount := links.Len()
	if parentSpanIDSet {
		refsCount++
	}

	refs := make([]OtelSpanRef, 0, refsCount)

	// Put parent span ID at the first place because usually backends look for it
	// as the first CHILD_OF item in the model.SpanRef slice.
	if parentSpanIDSet {

		refs = append(refs, OtelSpanRef{
			TraceId: traceID.HexString(),
			SpanId:  parentSpanID.HexString(),
			RefType: "CHILD_OF",
		})
	}

	for i := 0; i < links.Len(); i++ {
		link := links.At(i)

		refs = append(refs, OtelSpanRef{
			TraceId: link.TraceID().HexString(),
			SpanId:  link.SpanID().HexString(),

			// Since Jaeger RefType is not captured in internal data,
			// use SpanRefType_FOLLOWS_FROM by default.
			// SpanRefType_CHILD_OF supposed to be set only from parentSpanID.
			RefType: "FOLLOWS_FROM",
		})
	}

	return refs, nil
}

// ServiceNameForResource gets the service name for a specified Resource.
// TODO: Find a better package for this function.
func ServiceNameForResource(resource pdata.Resource) string {
	// if resource.IsNil() {
	// 	return "<nil-resource>"
	// }

	service, found := resource.Attributes().Get(conventions.AttributeServiceName)
	if !found {
		return "<nil-service-name>"
	}

	return service.StringVal()
}

func populateOtherDimensions(attributes pdata.AttributeMap, span *Span) {

	attributes.Range(func(k string, v pdata.AttributeValue) bool {
		if k == "http.status_code" {
			if v.IntVal() >= 400 {
				span.HasError = true
			}
			span.HttpCode = strconv.FormatInt(v.IntVal(), 10)
			span.ResponseStatusCode = span.HttpCode
		} else if k == "http.url" && span.Kind == 3 {
			value := v.StringVal()
			valueUrl, err := url.Parse(value)
			if err == nil {
				value = valueUrl.Hostname()
			}
			span.ExternalHttpUrl = value
		} else if k == "http.method" && span.Kind == 3 {
			span.ExternalHttpMethod = v.StringVal()
		} else if k == "http.url" && span.Kind != 3 {
			span.HttpUrl = v.StringVal()
		} else if k == "http.method" && span.Kind != 3 {
			span.HttpMethod = v.StringVal()
		} else if k == "http.route" {
			span.HttpRoute = v.StringVal()
		} else if k == "http.host" {
			span.HttpHost = v.StringVal()
		} else if k == "messaging.system" {
			span.MsgSystem = v.StringVal()
		} else if k == "messaging.operation" {
			span.MsgOperation = v.StringVal()
		} else if k == "component" {
			span.Component = v.StringVal()
		} else if k == "db.system" {
			span.DBSystem = v.StringVal()
		} else if k == "db.name" {
			span.DBName = v.StringVal()
		} else if k == "db.operation" {
			span.DBOperation = v.StringVal()
		} else if k == "peer.service" {
			span.PeerService = v.StringVal()
		} else if k == "rpc.grpc.status_code" {
			// Handle both string/int status code in GRPC spans.
			statusString, err := strconv.Atoi(v.StringVal())
			statusInt := v.IntVal()
			if err == nil && statusString != 0 {
				statusInt = int64(statusString)
			}
			if statusInt >= 2 {
				span.HasError = true
			}
			span.GRPCCode = strconv.FormatInt(statusInt, 10)
			span.ResponseStatusCode = span.GRPCCode
		} else if k == "rpc.method" {
			span.RPCMethod = v.StringVal()
			system, found := attributes.Get("rpc.system")
			if found && system.StringVal() == "grpc" {
				span.GRPCMethod = v.StringVal()
			}
		} else if k == "rpc.service" {
			span.RPCService = v.StringVal()
		} else if k == "rpc.system" {
			span.RPCSystem = v.StringVal()
		} else if k == "rpc.jsonrpc.error_code" {
			span.ResponseStatusCode = v.StringVal()
		}
		return true

	})
}

func populateEvents(events pdata.SpanEventSlice, span *Span) {
	for i := 0; i < events.Len(); i++ {
		event := Event{}
		event.Name = events.At(i).Name()
		event.TimeUnixNano = uint64(events.At(i).Timestamp())
		event.AttributeMap = map[string]string{}
		event.IsError = false
		events.At(i).Attributes().Range(func(k string, v pdata.AttributeValue) bool {
			if v.Type().String() == "INT" {
				event.AttributeMap[k] = strconv.FormatInt(v.IntVal(), 10)
			} else {
				event.AttributeMap[k] = v.StringVal()
			}
			return true
		})
		if event.Name == "exception" {
			event.IsError = true
			span.ErrorEvent = event
			uuidWithHyphen := uuid.New()
			uuid := strings.Replace(uuidWithHyphen.String(), "-", "", -1)
			span.ErrorID = uuid
			hmd5 := md5.Sum([]byte(span.ServiceName + span.ErrorEvent.AttributeMap["exception.type"] + span.ErrorEvent.AttributeMap["exception.message"]))
			span.ErrorGroupID = fmt.Sprintf("%x", hmd5)
		}
		stringEvent, _ := json.Marshal(event)
		span.Events = append(span.Events, string(stringEvent))
	}
}

func populateTraceModel(span *Span) {
	span.TraceModel.Events = span.Events
	span.TraceModel.HasError = span.HasError
}

func newStructuredSpan(otelSpan pdata.Span, ServiceName string, resource pdata.Resource) *Span {

	durationNano := uint64(otelSpan.EndTimestamp() - otelSpan.StartTimestamp())

	attributes := otelSpan.Attributes()
	resourceAttributes := resource.Attributes()
	tagMap := map[string]string{}

	attributes.Range(func(k string, v pdata.AttributeValue) bool {
		v.StringVal()
		if v.Type().String() == "INT" {
			tagMap[k] = strconv.FormatInt(v.IntVal(), 10)
		} else if v.StringVal() != "" {
			tagMap[k] = v.StringVal()
		}
		return true

	})

	resourceAttributes.Range(func(k string, v pdata.AttributeValue) bool {
		v.StringVal()
		if v.Type().String() == "INT" {
			tagMap[k] = strconv.FormatInt(v.IntVal(), 10)
		} else if v.StringVal() != "" {
			tagMap[k] = v.StringVal()
		}
		return true

	})

	references, _ := makeJaegerProtoReferences(otelSpan.Links(), otelSpan.ParentSpanID(), otelSpan.TraceID())

	var span *Span = &Span{
		TraceId:           otelSpan.TraceID().HexString(),
		SpanId:            otelSpan.SpanID().HexString(),
		ParentSpanId:      otelSpan.ParentSpanID().HexString(),
		Name:              otelSpan.Name(),
		StartTimeUnixNano: uint64(otelSpan.StartTimestamp()),
		DurationNano:      durationNano,
		ServiceName:       ServiceName,
		Kind:              int8(otelSpan.Kind()),
		StatusCode:        int16(otelSpan.Status().Code()),
		TagMap:            tagMap,
		HasError:          false,
		TraceModel: TraceModel{
			TraceId:           otelSpan.TraceID().HexString(),
			SpanId:            otelSpan.SpanID().HexString(),
			Name:              otelSpan.Name(),
			DurationNano:      durationNano,
			StartTimeUnixNano: uint64(otelSpan.StartTimestamp()),
			ServiceName:       ServiceName,
			Kind:              int8(otelSpan.Kind()),
			References:        references,
			TagMap:            tagMap,
			HasError:          false,
		},
	}

	if span.StatusCode == 2 {
		span.HasError = true
	}
	populateOtherDimensions(attributes, span)
	populateEvents(otelSpan.Events(), span)
	populateTraceModel(span)

	return span
}

// traceDataPusher implements OTEL exporterhelper.traceDataPusher

func (s *storage) write(ctx context.Context, structuredSpan *Span) error {
	// This is where we need to write span into pinot

	if s.spanKafkaWriter != nil {
		if err := s.writeModel(ctx, structuredSpan); err != nil {
			return err
		}
	}

	if s.indexKafkaWriter != nil {
		if err := s.writeIndex(ctx, structuredSpan); err != nil {
			return err
		}
	}

	if s.errorKafkaWriter != nil {
		if err := s.writeError(ctx, structuredSpan); err != nil {
			return err
		}
	}

	return nil
}

func (s *storage) writeModel(ctx context.Context, structuredSpan *Span) error {
	span, err := json.Marshal(structuredSpan.TraceModel)

	if err != nil {
		zap.S().Error("Error in writing spans to pinot: ", err)
		return err
	}

	data := map[string]interface{}{
		"timestamp": int64(structuredSpan.StartTimeUnixNano),
		"traceID":   structuredSpan.TraceId,
		"model":     string(span),
	}

	dataJsonBytes, dataMarshallErr := json.Marshal(data)

	if dataMarshallErr != nil {
		zap.S().Error("Error in writing spans to pinot: ", dataMarshallErr)
		return dataMarshallErr
	}

	kafkaWriteError := s.spanKafkaWriter.WriteMessages(ctx, kafka.Message{
		Key: []byte(strconv.Itoa(1)),
		// create an arbitrary message payload for the value
		Value: dataJsonBytes,
		Time:  time.Now(),
	})

	if kafkaWriteError != nil {
		zap.S().Error("Error in writing spans to pinot: ", kafkaWriteError)
		return kafkaWriteError
	}

	return nil
}

func (s *storage) writeIndex(ctx context.Context, structuredSpan *Span) error {
	data := map[string]interface{}{
		"timestamp":          int64(structuredSpan.StartTimeUnixNano),
		"traceID":            structuredSpan.TraceId,
		"spanID":             structuredSpan.SpanId,
		"parentSpanID":       structuredSpan.ParentSpanId,
		"serviceName":        structuredSpan.ServiceName,
		"name":               structuredSpan.Name,
		"kind":               structuredSpan.Kind,
		"durationNanos":      structuredSpan.DurationNano,
		"statusCode":         structuredSpan.StatusCode,
		"externalHttpMethod": structuredSpan.ExternalHttpMethod,
		"externalHttpUrl":    structuredSpan.ExternalHttpUrl,
		"component":          structuredSpan.Component,
		"dbSystem":           structuredSpan.DBSystem,
		"dbName":             structuredSpan.DBName,
		"dbOperation":        structuredSpan.DBOperation,
		"peerService":        structuredSpan.PeerService,
		"events":             structuredSpan.Events,
		"httpMethod":         structuredSpan.HttpMethod,
		"httpUrl":            structuredSpan.HttpUrl,
		"httpCode":           structuredSpan.HttpCode,
		"httpRoute":          structuredSpan.HttpRoute,
		"httpHost":           structuredSpan.HttpHost,
		"msgSystem":          structuredSpan.MsgSystem,
		"msgOperation":       structuredSpan.MsgOperation,
		"hasError":           structuredSpan.HasError,
		"tagMap":             structuredSpan.TagMap,
	}

	dataJsonBytes, dataMarshallErr := json.Marshal(data)

	if dataMarshallErr != nil {
		zap.S().Error("Error in writing spans to pinot: ", dataMarshallErr)
		return dataMarshallErr
	}

	kafkaWriteError := s.indexKafkaWriter.WriteMessages(ctx, kafka.Message{
		Key: []byte(strconv.Itoa(1)),
		// create an arbitrary message payload for the value
		Value: dataJsonBytes,
		Time:  time.Now(),
	})

	if kafkaWriteError != nil {
		zap.S().Error("Error in writing spans to pinot: ", kafkaWriteError)
		return kafkaWriteError
	}

	return nil
}

func (s *storage) writeError(ctx context.Context, structuredSpan *Span) error {

	if structuredSpan.ErrorEvent.Name == "" {
		return nil
	}

	data := map[string]interface{}{
		"timestamp":           int64(structuredSpan.ErrorEvent.TimeUnixNano),
		"errorID":             structuredSpan.ErrorID,
		"groupID":             structuredSpan.ErrorGroupID,
		"traceID":             structuredSpan.TraceId,
		"spanID":              structuredSpan.SpanId,
		"serviceName":         structuredSpan.ServiceName,
		"exceptionType":       structuredSpan.ErrorEvent.AttributeMap["exception.type"],
		"exceptionMessage":    structuredSpan.ErrorEvent.AttributeMap["exception.message"],
		"exceptionStacktrace": structuredSpan.ErrorEvent.AttributeMap["exception.stacktrace"],
		"exceptionEscaped":    stringToBool(structuredSpan.ErrorEvent.AttributeMap["exception.escaped"]),
	}

	dataJsonBytes, dataMarshallErr := json.Marshal(data)

	if dataMarshallErr != nil {
		zap.S().Error("Error in writing spans to pinot: ", dataMarshallErr)
		return dataMarshallErr
	}

	kafkaWriteError := s.errorKafkaWriter.WriteMessages(ctx, kafka.Message{
		Key: []byte(strconv.Itoa(1)),
		// create an arbitrary message payload for the value
		Value: dataJsonBytes,
		Time:  time.Now(),
	})

	if kafkaWriteError != nil {
		zap.S().Error("Error in writing spans to pinot: ", kafkaWriteError)
		return kafkaWriteError
	}

	return nil
}

func stringToBool(s string) bool {
	if strings.ToLower(s) == "true" {
		return true
	}
	return false
}

func (s *storage) pushTraceData(ctx context.Context, td pdata.Traces) error {

	rss := td.ResourceSpans()
	for i := 0; i < rss.Len(); i++ {
		// fmt.Printf("ResourceSpans #%d\n", i)
		rs := rss.At(i)

		serviceName := ServiceNameForResource(rs.Resource())

		ilss := rs.InstrumentationLibrarySpans()
		for j := 0; j < ilss.Len(); j++ {
			// fmt.Printf("InstrumentationLibrarySpans #%d\n", j)
			ils := ilss.At(j)

			spans := ils.Spans()

			for k := 0; k < spans.Len(); k++ {
				span := spans.At(k)
				// traceID := hex.EncodeToString(span.TraceID())
				structuredSpan := newStructuredSpan(span, serviceName, rs.Resource())
				err := s.write(ctx, structuredSpan)
				if err != nil {
					zap.S().Error("Error in writing spans to pinot: ", err)
				}
			}
		}
	}

	return nil
}
