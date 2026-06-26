# Config Parser Benchmark

`config-parser-bench` measures DAE configuration parsing without starting DAE,
loading eBPF, or constructing DNS and routing control planes.

The token stream is fully populated before `parser.Start()` is timed. The
reported `parser_start` duration therefore excludes file reads, tokenization,
and parse-tree walking.

## Run Against A File

```bash
go run ./cmd/config-parser-bench \
  -config /path/to/routing.dae \
  -mode ll \
  -runs 5
```

Run SLL in a separate process so it does not share ANTLR DFA state with the LL
measurement:

```bash
go run ./cmd/config-parser-bench \
  -config /path/to/routing.dae \
  -mode sll \
  -runs 5
```

Within one invocation, run 1 is the coldest measurement. Later runs reuse the
ANTLR-generated parser's process-wide DFA state and represent warm parsing.

## Synthetic Scaling

```bash
go run ./cmd/config-parser-bench \
  -synthetic-routing-rules 2067 \
  -mode ll \
  -runs 5 \
  -walk=false
```

Use `-format json` for machine-readable output.
