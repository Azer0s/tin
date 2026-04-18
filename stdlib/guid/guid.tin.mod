{
  "package": "guid",
  "functions": [
    {
      "name": "new",
      "irName": "guid__new",
      "params": null,
      "retType": "Guid"
    }
  ],
  "structs": [
    {
      "name": "Guid",
      "irName": "Guid",
      "fields": [
        {
          "name": "value",
          "type": "string"
        }
      ],
      "methods": [
        {
          "name": "to_json",
          "irName": "Guid_to_json",
          "params": [
            {
              "name": "this",
              "type": "Guid"
            }
          ],
          "retType": "string"
        },
        {
          "name": "apply_json",
          "irName": "Guid_apply_json",
          "params": [
            {
              "name": "this",
              "type": "*Guid"
            },
            {
              "name": "s",
              "type": "string"
            }
          ],
          "retType": "bool"
        },
        {
          "name": "string",
          "irName": "Guid_string",
          "params": [
            {
              "name": "this",
              "type": "Guid"
            }
          ],
          "retType": "string"
        }
      ]
    }
  ]
}