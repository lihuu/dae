# Local dae_config Parser Override

This directory is a local DAE-maintained override of
`github.com/daeuniverse/dae-config-dist/go/dae_config` for the parser performance
investigation and local fix.

## Source grammar

The grammar source used for this generated package is:

- `third_party/dae_config/dae_config.g4`

Local change from upstream `dae-config-dist/main`:

```antlr
input
    : programStructureBlcok*
    ;
```

The upstream grammar used a left-recursive rule with an epsilon alternative:

```antlr
input
    : programStructureBlcok
    | input programStructureBlcok
    | // empty
    ;
```

That form causes the generated Go parser to spend 13-28 seconds in `Start()` for
production-shaped routing-only files. The `programStructureBlcok*` form accepts
the same top-level language while generating a simple loop in `Input()`.

## Generation

Generated with ANTLR 4.12.0 to keep compatibility with the existing Go runtime
import path used by DAE:

```bash
ANTLR_JAR=/Users/lihu/git/dae-config/.cache/antlr/antlr-4.12.0-complete.jar \
  third_party/dae_config/generate.sh
```

ANTLR 4.13.x generates code for `github.com/antlr4-go/antlr/v4` and renames the
entry method to `Start_()`, so it is intentionally not used here.

## Local wiring

The parent DAE `go.mod` contains:

```go
replace github.com/daeuniverse/dae-config-dist/go/dae_config => ./third_party/dae_config
```

This is a self-maintained local override. It is not an upstream patch.
