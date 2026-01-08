{
  "package": "io",
  "functions": [
    {
      "name": "puts",
      "irName": "io__puts",
      "externName": "puts",
      "params": [{"name": "s", "type": "string"}],
      "retType": ""
    },
    {
      "name": "fputs",
      "irName": "io__fputs",
      "externName": "fputs",
      "params": [{"name": "s", "type": "string"}, {"name": "stream", "type": "*void"}],
      "retType": "i32"
    },
    {
      "name": "fprintf",
      "irName": "io__fprintf",
      "externName": "fprintf",
      "params": [{"name": "stream", "type": "*void"}, {"name": "fmt", "type": "string"}],
      "retType": "i32",
      "variadic": true
    },
    {
      "name": "printf",
      "irName": "io__printf",
      "externName": "printf",
      "params": [{"name": "fmt", "type": "string"}],
      "retType": "i32",
      "variadic": true
    },
    {
      "name": "fgets",
      "irName": "io__fgets",
      "externName": "fgets",
      "params": [{"name": "buf", "type": "*char"}, {"name": "n", "type": "i32"}, {"name": "stream", "type": "*void"}],
      "retType": "*char"
    },
    {
      "name": "scanf",
      "irName": "io__scanf",
      "externName": "scanf",
      "params": [{"name": "fmt", "type": "string"}],
      "retType": "i32",
      "variadic": true
    },
    {
      "name": "fopen",
      "irName": "io__fopen",
      "externName": "fopen",
      "params": [{"name": "path", "type": "string"}, {"name": "mode", "type": "string"}],
      "retType": "*void"
    },
    {
      "name": "fclose",
      "irName": "io__fclose",
      "externName": "fclose",
      "params": [{"name": "stream", "type": "*void"}],
      "retType": "i32"
    },
    {
      "name": "fread",
      "irName": "io__fread",
      "externName": "fread",
      "params": [{"name": "buf", "type": "*void"}, {"name": "size", "type": "i64"}, {"name": "count", "type": "i64"}, {"name": "stream", "type": "*void"}],
      "retType": "i64"
    },
    {
      "name": "fwrite",
      "irName": "io__fwrite",
      "externName": "fwrite",
      "params": [{"name": "buf", "type": "*void"}, {"name": "size", "type": "i64"}, {"name": "count", "type": "i64"}, {"name": "stream", "type": "*void"}],
      "retType": "i64"
    },
    {
      "name": "fseek",
      "irName": "io__fseek",
      "externName": "fseek",
      "params": [{"name": "stream", "type": "*void"}, {"name": "offset", "type": "i64"}, {"name": "whence", "type": "i32"}],
      "retType": "i32"
    },
    {
      "name": "ftell",
      "irName": "io__ftell",
      "externName": "ftell",
      "params": [{"name": "stream", "type": "*void"}],
      "retType": "i64"
    },
    {
      "name": "fflush",
      "irName": "io__fflush",
      "externName": "fflush",
      "params": [{"name": "stream", "type": "*void"}],
      "retType": "i32"
    },
    {
      "name": "feof",
      "irName": "io__feof",
      "externName": "feof",
      "params": [{"name": "stream", "type": "*void"}],
      "retType": "i32"
    },
    {
      "name": "ferror",
      "irName": "io__ferror",
      "externName": "ferror",
      "params": [{"name": "stream", "type": "*void"}],
      "retType": "i32"
    },
    {
      "name": "remove",
      "irName": "io__remove",
      "externName": "remove",
      "params": [{"name": "path", "type": "string"}],
      "retType": "i32"
    },
    {
      "name": "rename",
      "irName": "io__rename",
      "externName": "rename",
      "params": [{"name": "old_path", "type": "string"}, {"name": "new_path", "type": "string"}],
      "retType": "i32"
    }
  ]
}
