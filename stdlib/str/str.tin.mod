{
  "package": "str",
  "functions": [
    {"name": "strlen",    "irName": "str__strlen",    "externName": "strlen",    "params": [{"name": "s", "type": "string"}], "retType": "i64"},
    {"name": "strcmp",    "irName": "str__strcmp",    "externName": "strcmp",    "params": [{"name": "a", "type": "string"}, {"name": "b", "type": "string"}], "retType": "i32"},
    {"name": "strncmp",   "irName": "str__strncmp",   "externName": "strncmp",   "params": [{"name": "a", "type": "string"}, {"name": "b", "type": "string"}, {"name": "n", "type": "i64"}], "retType": "i32"},
    {"name": "strcasecmp","irName": "str__strcasecmp","externName": "strcasecmp","params": [{"name": "a", "type": "string"}, {"name": "b", "type": "string"}], "retType": "i32"},
    {"name": "strchr",    "irName": "str__strchr",    "externName": "strchr",    "params": [{"name": "s", "type": "string"}, {"name": "c", "type": "i32"}], "retType": "*char"},
    {"name": "strrchr",   "irName": "str__strrchr",   "externName": "strrchr",   "params": [{"name": "s", "type": "string"}, {"name": "c", "type": "i32"}], "retType": "*char"},
    {"name": "strstr",    "irName": "str__strstr",    "externName": "strstr",    "params": [{"name": "haystack", "type": "string"}, {"name": "needle", "type": "string"}], "retType": "*char"},
    {"name": "strcpy",    "irName": "str__strcpy",    "externName": "strcpy",    "params": [{"name": "dst", "type": "*char"}, {"name": "src", "type": "string"}], "retType": "*char"},
    {"name": "strncpy",   "irName": "str__strncpy",   "externName": "strncpy",   "params": [{"name": "dst", "type": "*char"}, {"name": "src", "type": "string"}, {"name": "n", "type": "i64"}], "retType": "*char"},
    {"name": "strcat",    "irName": "str__strcat",    "externName": "strcat",    "params": [{"name": "dst", "type": "*char"}, {"name": "src", "type": "string"}], "retType": "*char"},
    {"name": "strdup",    "irName": "str__strdup",    "externName": "strdup",    "params": [{"name": "s", "type": "string"}], "retType": "string"},
    {"name": "atoi",      "irName": "str__atoi",      "externName": "atoi",      "params": [{"name": "s", "type": "string"}], "retType": "i32"},
    {"name": "atol",      "irName": "str__atol",      "externName": "atol",      "params": [{"name": "s", "type": "string"}], "retType": "i64"},
    {"name": "atof",      "irName": "str__atof",      "externName": "atof",      "params": [{"name": "s", "type": "string"}], "retType": "f64"},
    {"name": "strtol",    "irName": "str__strtol",    "externName": "strtol",    "params": [{"name": "s", "type": "string"}, {"name": "endptr", "type": "**char"}, {"name": "base", "type": "i32"}], "retType": "i64"},
    {"name": "strtod",    "irName": "str__strtod",    "externName": "strtod",    "params": [{"name": "s", "type": "string"}, {"name": "endptr", "type": "**char"}], "retType": "f64"},
    {"name": "sprintf",   "irName": "str__sprintf",   "externName": "sprintf",   "params": [{"name": "buf", "type": "*char"}, {"name": "fmt", "type": "string"}], "retType": "i32", "variadic": true}
  ]
}
