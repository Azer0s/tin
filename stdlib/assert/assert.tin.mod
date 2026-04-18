{
  "package": "assert",
  "functions": [
    {
      "name": "ok",
      "irName": "assert__ok",
      "params": [
        {
          "name": "cond",
          "type": "bool"
        }
      ],
      "retType": ""
    },
    {
      "name": "not_ok",
      "irName": "assert__not_ok",
      "params": [
        {
          "name": "cond",
          "type": "bool"
        }
      ],
      "retType": ""
    },
    {
      "name": "equals",
      "irName": "assert__equals",
      "params": [
        {
          "name": "expected",
          "type": "t"
        },
        {
          "name": "actual",
          "type": "t"
        }
      ],
      "retType": ""
    },
    {
      "name": "not_equals",
      "irName": "assert__not_equals",
      "params": [
        {
          "name": "a",
          "type": "i64"
        },
        {
          "name": "b",
          "type": "i64"
        }
      ],
      "retType": ""
    },
    {
      "name": "fails",
      "irName": "assert__fails",
      "params": [
        {
          "name": "msg",
          "type": "string"
        }
      ],
      "retType": ""
    },
    {
      "name": "panics",
      "irName": "assert__panics",
      "params": [
        {
          "name": "f",
          "type": "fn() void"
        }
      ],
      "retType": ""
    }
  ]
}