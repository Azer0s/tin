{
  "package": "errors",
  "functions": [
    {
      "name": "new",
      "irName": "errors__new",
      "params": [
        {
          "name": "msg",
          "type": "string"
        }
      ],
      "retType": "Err"
    },
    {
      "name": "wrap",
      "irName": "errors__wrap",
      "params": [
        {
          "name": "inner",
          "type": "Err"
        },
        {
          "name": "msg",
          "type": "string"
        }
      ],
      "retType": "Err"
    },
    {
      "name": "has",
      "irName": "errors__has",
      "params": [
        {
          "name": "err",
          "type": "Err"
        }
      ],
      "retType": "bool"
    },
    {
      "name": "equals",
      "irName": "errors__equals",
      "params": [
        {
          "name": "a",
          "type": "Err"
        },
        {
          "name": "b",
          "type": "Err"
        }
      ],
      "retType": "bool"
    }
  ],
  "structs": [
    {
      "name": "Error",
      "irName": "Error",
      "fields": [
        {
          "name": "_msg",
          "type": "string"
        }
      ],
      "methods": [
        {
          "name": "message",
          "irName": "Error_message",
          "params": [
            {
              "name": "this",
              "type": "Error"
            }
          ],
          "retType": "string"
        },
        {
          "name": "print",
          "irName": "Error_print",
          "params": [
            {
              "name": "this",
              "type": "Error"
            }
          ],
          "retType": "string"
        }
      ]
    }
  ],
  "types": [
    {
      "name": "Err",
      "target": "*Error"
    }
  ]
}