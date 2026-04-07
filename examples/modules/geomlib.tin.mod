{
  "package": "geomlib",
  "functions": [
    {
      "name": "make_point",
      "irName": "geomlib__make_point",
      "params": [
        {
          "name": "x",
          "type": "i64"
        },
        {
          "name": "y",
          "type": "i64"
        }
      ],
      "retType": "point"
    },
    {
      "name": "midpoint",
      "irName": "geomlib__midpoint",
      "params": [
        {
          "name": "a",
          "type": "point"
        },
        {
          "name": "b",
          "type": "point"
        }
      ],
      "retType": "point"
    }
  ],
  "structs": [
    {
      "name": "point",
      "irName": "point",
      "fields": [
        {
          "name": "x",
          "type": "i64"
        },
        {
          "name": "y",
          "type": "i64"
        }
      ],
      "methods": [
        {
          "name": "show",
          "irName": "point_show",
          "params": [
            {
              "name": "this",
              "type": "point"
            }
          ],
          "retType": "string"
        },
        {
          "name": "distance_sq",
          "irName": "point_distance_sq",
          "params": [
            {
              "name": "this",
              "type": "point"
            },
            {
              "name": "other",
              "type": "point"
            }
          ],
          "retType": "i64"
        }
      ]
    }
  ]
}