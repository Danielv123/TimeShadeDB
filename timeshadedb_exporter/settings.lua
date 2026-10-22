data:extend({
  {
    type = "int-setting",
    name = "timeshadedb-tile-rescan-interval",
    setting_type = "runtime-global",
    default_value = 3600,
    minimum_value = 1,
    maximum_value = 216000
  },
  {
    type = "int-setting",
    name = "timeshadedb-tile-export-chunks-per-tick",
    setting_type = "runtime-global",
    default_value = 4,
    minimum_value = 1,
    maximum_value = 256
  }
})
