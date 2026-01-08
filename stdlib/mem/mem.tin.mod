{
  "package": "mem",
  "functions": [
    {"name": "malloc",  "irName": "mem__malloc",  "externName": "malloc",  "params": [{"name": "size", "type": "i64"}], "retType": "*void"},
    {"name": "calloc",  "irName": "mem__calloc",  "externName": "calloc",  "params": [{"name": "n", "type": "i64"}, {"name": "size", "type": "i64"}], "retType": "*void"},
    {"name": "realloc", "irName": "mem__realloc", "externName": "realloc", "params": [{"name": "ptr", "type": "*void"}, {"name": "new_size", "type": "i64"}], "retType": "*void"},
    {"name": "free",    "irName": "mem__free",    "externName": "free",    "params": [{"name": "ptr", "type": "*void"}], "retType": ""},
    {"name": "memcpy",  "irName": "mem__memcpy",  "externName": "memcpy",  "params": [{"name": "dst", "type": "*void"}, {"name": "src", "type": "*void"}, {"name": "n", "type": "i64"}], "retType": "*void"},
    {"name": "memmove", "irName": "mem__memmove", "externName": "memmove", "params": [{"name": "dst", "type": "*void"}, {"name": "src", "type": "*void"}, {"name": "n", "type": "i64"}], "retType": "*void"},
    {"name": "memset",  "irName": "mem__memset",  "externName": "memset",  "params": [{"name": "ptr", "type": "*void"}, {"name": "c", "type": "i32"}, {"name": "n", "type": "i64"}], "retType": "*void"},
    {"name": "memcmp",  "irName": "mem__memcmp",  "externName": "memcmp",  "params": [{"name": "a", "type": "*void"}, {"name": "b", "type": "*void"}, {"name": "n", "type": "i64"}], "retType": "i32"}
  ]
}
