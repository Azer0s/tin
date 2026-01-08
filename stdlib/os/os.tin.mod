{
  "package": "os",
  "functions": [
    {"name": "exit",    "irName": "os__exit",    "externName": "exit",    "params": [{"name": "code", "type": "i32"}], "retType": ""},
    {"name": "abort",   "irName": "os__abort",   "externName": "abort",   "params": [], "retType": ""},
    {"name": "system",  "irName": "os__system",  "externName": "system",  "params": [{"name": "cmd", "type": "string"}], "retType": "i32"},
    {"name": "getenv",  "irName": "os__getenv",  "externName": "getenv",  "params": [{"name": "name", "type": "string"}], "retType": "string"},
    {"name": "setenv",  "irName": "os__setenv",  "externName": "setenv",  "params": [{"name": "name", "type": "string"}, {"name": "value", "type": "string"}, {"name": "overwrite", "type": "i32"}], "retType": "i32"},
    {"name": "unsetenv","irName": "os__unsetenv","externName": "unsetenv","params": [{"name": "name", "type": "string"}], "retType": "i32"},
    {"name": "getpid",  "irName": "os__getpid",  "externName": "getpid",  "params": [], "retType": "i32"},
    {"name": "getppid", "irName": "os__getppid", "externName": "getppid", "params": [], "retType": "i32"}
  ]
}
