package metric

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/instrument"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/metric/internal/asyncstate"
	"go.opentelemetry.io/otel/sdk/metric/internal/viewstate"
	"go.opentelemetry.io/otel/sdk/metric/number"
	"go.opentelemetry.io/otel/sdk/metric/reader"
	"go.opentelemetry.io/otel/sdk/metric/sdkinstrument"
	"go.opentelemetry.io/otel/sdk/metric/views"
	"go.opentelemetry.io/otel/sdk/resource"
)

type (
	Config struct {
		res     *resource.Resource
		readers []*reader.Reader
		views   []views.View
	}

	Option func(cfg *Config)

	provider struct {
		cfg       Config
		startTime time.Time
		lock      sync.Mutex
		ordered   []*meter
		meters    map[instrumentation.Library]*meter
	}

	providerProducer struct {
		lock        sync.Mutex
		provider    *provider
		reader      *reader.Reader
		lastCollect time.Time
	}

	instrumentIface interface {
		Descriptor() sdkinstrument.Descriptor
		Collect(r *reader.Reader, seq reader.Sequence, output *[]reader.Instrument)
	}

	meter struct {
		library  instrumentation.Library
		provider *provider
		names    map[string][]instrumentIface
		views    *viewstate.Compiler

		lock        sync.Mutex
		instruments []instrumentIface
		callbacks   []*asyncstate.Callback
	}
)

var (
	_ metric.Meter = &meter{}
)

func WithResource(res *resource.Resource) Option {
	return func(cfg *Config) {
		cfg.res = res
	}
}

func WithReader(r *reader.Reader) Option {
	return func(cfg *Config) {
		cfg.readers = append(cfg.readers, r)
	}
}

func WithViews(vs ...views.View) Option {
	return func(cfg *Config) {
		cfg.views = append(cfg.views, vs...)
	}
}

func New(opts ...Option) metric.MeterProvider {
	cfg := Config{
		res: resource.Default(),
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	p := &provider{
		cfg:       cfg,
		startTime: time.Now(),
		meters:    map[instrumentation.Library]*meter{},
	}
	for _, reader := range cfg.readers {
		reader.Exporter().Register(p.producerFor(reader))
	}
	return p
}

func (p *provider) producerFor(r *reader.Reader) reader.Producer {
	return &providerProducer{
		provider: p,
		reader:   r,
	}
}

func (pp *providerProducer) Produce() reader.Metrics {
	pp.lock.Lock()
	defer pp.lock.Unlock()

	ordered := pp.provider.getOrdered()

	output := reader.Metrics{
		Resource: pp.provider.cfg.res,
		Scopes:   make([]reader.Scope, len(ordered)),
	}

	sequence := reader.Sequence{
		Start: pp.provider.startTime,
		Last:  pp.lastCollect,
		Now:   time.Now(),
	}

	// TODO: Add a timeout to the context.
	ctx := context.Background()

	for meterIdx, meter := range ordered {
		// Lock
		meter.lock.Lock()
		callbacks := meter.callbacks
		instruments := meter.instruments
		meter.lock.Unlock()

		for _, cb := range callbacks {
			cb.Run(ctx, pp.reader)
		}

		output.Scopes[meterIdx].Library = meter.library

		for _, inst := range instruments {
			inst.Collect(pp.reader, sequence, &output.Scopes[meterIdx].Instruments)
		}
	}

	pp.lastCollect = sequence.Now

	return output
}

func (p *provider) getOrdered() []*meter {
	p.lock.Lock()
	defer p.lock.Unlock()
	return p.ordered
}

func (p *provider) Meter(name string, opts ...metric.MeterOption) metric.Meter {
	cfg := metric.NewMeterConfig(opts...)
	lib := instrumentation.Library{
		Name:      name,
		Version:   cfg.InstrumentationVersion(),
		SchemaURL: cfg.SchemaURL(),
	}

	p.lock.Lock()
	defer p.lock.Unlock()

	m := p.meters[lib]
	if m != nil {
		return m
	}
	m = &meter{
		provider: p,
		library:  lib,
		names:    map[string][]instrumentIface{},
		views:    viewstate.New(lib, p.cfg.views, p.cfg.readers),
	}
	p.ordered = append(p.ordered, m)
	p.meters[lib] = m
	return m
}

func (m *meter) RegisterCallback(insts []instrument.Asynchronous, function func(context.Context)) error {
	cb, err := asyncstate.NewCallback(insts, function)

	if err == nil {
		m.lock.Lock()
		defer m.lock.Unlock()
		m.callbacks = append(m.callbacks, cb)
	}
	return err
}

func nameLookup[T instrumentIface](
	m *meter,
	name string,
	opts []instrument.Option,
	nk number.Kind,
	ik sdkinstrument.Kind,
	f func(desc sdkinstrument.Descriptor) T,
) (T, error) {
	cfg := instrument.NewConfig(opts...)
	desc := sdkinstrument.NewDescriptor(name, ik, nk, cfg.Description(), cfg.Unit())

	m.lock.Lock()
	defer m.lock.Unlock()
	lookup := m.names[name]

	for _, found := range lookup {
		match, ok := found.(T)
		if !ok {
			continue
		}

		exist := found.Descriptor()

		if exist.NumberKind != nk || exist.Kind != ik || exist.Unit != cfg.Unit() {
			continue
		}

		// Exact match (ignores description)
		return match, nil
	}
	value := f(desc)
	m.names[name] = append(m.names[name], value)
	return value, nil
}