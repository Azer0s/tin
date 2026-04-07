{
  "package": "macros",
  "macros": [
    {
      "name": "loop",
      "body": "for true",
      "tags": [
        "no_parens",
        "no_excl"
      ]
    },
    {
      "name": "async",
      "body": "fn{#async}",
      "tags": [
        "no_parens",
        "no_excl"
      ]
    },
    {
      "name": "todo",
      "body": "_tin_panic(\"not yet implemented\")"
    },
    {
      "name": "unreachable",
      "body": "_tin_panic(\"unreachable\")"
    },
    {
      "name": "min",
      "body": "",
      "params": [
        "a",
        "b"
      ]
    },
    {
      "name": "max",
      "body": "",
      "params": [
        "a",
        "b"
      ]
    },
    {
      "name": "abs",
      "body": "",
      "params": [
        "x"
      ]
    },
    {
      "name": "clamp",
      "body": "",
      "params": [
        "x",
        "lo",
        "hi"
      ]
    },
    {
      "name": "dbg",
      "body": "x",
      "params": [
        "x"
      ]
    }
  ]
}