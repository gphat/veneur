# Simple Sensor format

The Simple Sensor Format — or SSF for short — is a language agnostic format for transmitting observability data such as trace spans, metrics, events and more.

# Why?

SSF is based on prior art of metrics formats and protocols mentioned in the [inspiration section](#inspiration). Unlike each of these wonderful formats, which we have used for years and benefited greatly from, SSF is a binary format utilizing [Protocol Buffers](https://developers.google.com/protocol-buffers/). It is emission, collection and storage agnostic. It is merely a protocol for transmitting information. It is developed as part of a suite of technologies around [Veneur](https://github.com/stripe/veneur).

## Why A New Format?

Because we want to:

* leverage protobuf so we no longer have to write buggy, string-based marshaling and unmarshaling code in clients libraries and server imeplementations
* benefit from the efficiency of protobuf
* collect and combine the great ideas from our [inspiration](https://github.com/stripe/veneur/tree/master/ssf#inspiration).
* add some of [our own ideas](https://github.com/stripe/veneur/tree/master/ssf#philosophy)

# Philosophy

We've got some novel ideas that we've put in to SSF. It might help to be familiar with the concepts in our [inspiration](https://github.com/stripe/veneur/tree/master/ssf#inspiration) Here they are:

* a timer is a span
* a log line is a span, especially if it's [structured](https://www.thoughtworks.com/radar/techniques/structured-logging)
* events are also spans
* therefore, the core unit of all observability data is a span, or a unit of a trace.
  * spans might be single units, or units of a larger whole
* other point metrics (e.g. counters and gauges) can be constituents of a span
  * it's more valuable to know the depth of a queue in the context of a span than alone
  * improve the context of counters and gauges, as they are part of a span
* provide a format containing the superset of many backend's features

# Structure

As with \*StatsD libraries, users will never muck with the protobuf. This will be handled by SSF clients! There are two high level classes of client: Those that use traditional signgle metrics per packet and those that collect metrics into a span and emit each span.

## Simple, A Metric

This matches the style of most existing \*StatsD style clients. An implementation would translate a call like:

```go
client.Increment("foo.queries_executed", map[string]string{"table_name", "customers"})
```

into code that creates and emits the following by marshaling to protobuf and UDP transmission:

```go
counter := ssf.SSFSample{
  Name:   "foo.queries_executed",
  Metric: ssf.SSFSample_COUNTER,
  Value:  1.0,
  Tags: map[string]string{
    "table_name": "customers",
  },
}
span := ssf.SSFSpan{
  Metrics: []*ssf.SSFSample{&counter},
}
```

## Full, A Span With Metrics

TODO

```go
counter := SSFSample{
  Name:   "foo.queries_executed",
  Metric: SSFSample_COUNTER,
  Value:  1.0,
  Tags: map[string]string{
    "table_name": "customers",
  },
}
span := SSFSpan{
  TraceId:        1234,
  Id:             1235,
  StartTimestamp: 0,
  EndTimestamp:   100,
  Metrics:        []*SSFSample{&counter},
}
```

# Inspiration

We build on the shoulders of giants, and are proud to have used and been inspired by these marvelous tools:

* [DogStatsD](http://docs.datadoghq.com/guides/dogstatsd/#datagram-format)
* [Metrics 2.0](http://metrics20.org)
* [OpenTracing](http://opentracing.io)
* [StatsD](https://github.com/b/statsd_spec)
