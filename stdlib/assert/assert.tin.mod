{
  "package": "assert",
  "functions": [
    {
      "name": "ok",
      "irName": "assert__ok",
      "externName": "_tin_assert_ok",
      "params": [{"name": "cond", "type": "bool"}],
      "retType": ""
    },
    {
      "name": "not_ok",
      "irName": "assert__not_ok",
      "externName": "_tin_assert_not_ok",
      "params": [{"name": "cond", "type": "bool"}],
      "retType": ""
    },
    {
      "name": "equals",
      "irName": "assert__equals",
      "externName": "_tin_assert_equals_i64",
      "params": [
        {"name": "expected", "type": "i64"},
        {"name": "actual",   "type": "i64"}
      ],
      "retType": ""
    },
    {
      "name": "not_equals",
      "irName": "assert__not_equals",
      "externName": "_tin_assert_not_equals_i64",
      "params": [
        {"name": "a", "type": "i64"},
        {"name": "b", "type": "i64"}
      ],
      "retType": ""
    },
    {
      "name": "equals_str",
      "irName": "assert__equals_str",
      "externName": "_tin_assert_equals_str",
      "params": [
        {"name": "expected", "type": "string"},
        {"name": "actual",   "type": "string"}
      ],
      "retType": ""
    },
    {
      "name": "equals_f64",
      "irName": "assert__equals_f64",
      "externName": "_tin_assert_equals_f64",
      "params": [
        {"name": "expected", "type": "f64"},
        {"name": "actual",   "type": "f64"}
      ],
      "retType": ""
    },
    {
      "name": "fails",
      "irName": "assert__fails",
      "externName": "_tin_assert_fail",
      "params": [{"name": "msg", "type": "string"}],
      "retType": ""
    }
  ]
}
