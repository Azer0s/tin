{
  "package": "ioutil",
  "functions": [
    {
      "name": "read_string",
      "irName": "ioutil__read_string",
      "params": [
        {
          "name": "r",
          "type": "io::AsyncReader"
        }
      ],
      "retType": "Future[string]"
    },
    {
      "name": "read_string_until",
      "irName": "ioutil__read_string_until",
      "params": [
        {
          "name": "r",
          "type": "io::AsyncReader"
        },
        {
          "name": "delim",
          "type": "byte"
        }
      ],
      "retType": "Future[string]"
    },
    {
      "name": "write_string",
      "irName": "ioutil__write_string",
      "params": [
        {
          "name": "w",
          "type": "io::AsyncWriter"
        },
        {
          "name": "s",
          "type": "string"
        }
      ],
      "retType": "Future[sync::Unit]"
    },
    {
      "name": "read_bytes",
      "irName": "ioutil__read_bytes",
      "params": [
        {
          "name": "r",
          "type": "io::AsyncReader"
        },
        {
          "name": "n",
          "type": "i64"
        }
      ],
      "retType": "Future[[byte]]"
    },
    {
      "name": "read_bytes_until",
      "irName": "ioutil__read_bytes_until",
      "params": [
        {
          "name": "r",
          "type": "io::AsyncReader"
        },
        {
          "name": "delim",
          "type": "byte"
        }
      ],
      "retType": "Future[[byte]]"
    },
    {
      "name": "write_bytes",
      "irName": "ioutil__write_bytes",
      "params": [
        {
          "name": "w",
          "type": "io::AsyncWriter"
        },
        {
          "name": "data",
          "type": "[byte]"
        }
      ],
      "retType": "Future[sync::Unit]"
    }
  ]
}