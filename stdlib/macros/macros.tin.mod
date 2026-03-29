{
  "package": "macros",
  "macros": [
    {
      "name": "loop",
      "body": "for true",
      "tags": ["no_parens", "no_excl"]
    },
    {
      "name": "async",
      "body": "fn{#async}",
      "tags": ["no_parens", "no_excl"]
    },
    {
      "name": "todo",
      "body": "_tin_panic(\"not yet implemented\")",
      "params": []
    },
    {
      "name": "unreachable",
      "body": "_tin_panic(\"unreachable\")",
      "params": []
    },
    {
      "name": "dbg",
      "body": "x",
      "params": ["x"]
    },
    {
      "name": "min",
      "body": "a < b ? a : b",
      "params": ["a", "b"]
    },
    {
      "name": "max",
      "body": "a > b ? a : b",
      "params": ["a", "b"]
    },
    {
      "name": "abs",
      "body": "x < 0 ? -x : x",
      "params": ["x"]
    },
    {
      "name": "clamp",
      "body": "x < lo ? lo : (x > hi ? hi : x)",
      "params": ["x", "lo", "hi"]
    }
  ]
}
