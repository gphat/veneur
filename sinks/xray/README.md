# X-Ray Sink

This sink sends Veneur metrics to [AWS X-Ray](https://aws.amazon.com/xray/).

# Configuration

See the various `xray_*` keys in [example.yaml](https://github.com/stripe/veneur/blob/master/example.yaml) for all available configuration options.

# Status

**This sink is experimental**.

# Capabilities

## Spans

Enabled if `xray_address` and `xray_sample_percentage` are set to non-empty
values.

Spans are sent to a local [AWS X-Ray Daemon](https://docs.aws.amazon.com/xray/latest/devguide/xray-daemon.html). The following
rules manage how [SSF](https://github.com/stripe/veneur/tree/master/ssf) spans
and tags are mapped to X-Ray [segment documents](https://docs.aws.amazon.com/xray/latest/devguide/xray-api-segmentdocuments.html#api-segmentdocuments-http):

* The SSF field `service` is mapped to the segment's `name` with invalid characters replaced with `_`.
* All the SSF tags are added as segment `annotations`.
* The `service` and `name` of the segment will be added as `http.request.url` separated by a `:`.
