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
  },
  {
    type = "int-setting",
    name = "timeshadedb-entity-sample-interval",
    setting_type = "runtime-global",
    default_value = 30,
    minimum_value = 1,
    maximum_value = 3600
  },
  {
    type = "int-setting",
    name = "timeshadedb-entity-max-silent-ticks",
    setting_type = "runtime-global",
    default_value = 300,
    minimum_value = 1,
    maximum_value = 216000
  },
  {
    type = "double-setting",
    name = "timeshadedb-entity-position-epsilon",
    setting_type = "runtime-global",
    default_value = 0.05,
    minimum_value = 0.0,
    maximum_value = 100.0
  },
  {
    type = "double-setting",
    name = "timeshadedb-entity-velocity-epsilon",
    setting_type = "runtime-global",
    default_value = 0.0025,
    minimum_value = 0.0,
    maximum_value = 10.0
  },
  {
    type = "double-setting",
    name = "timeshadedb-entity-orientation-epsilon",
    setting_type = "runtime-global",
    default_value = 0.002,
    minimum_value = 0.0,
    maximum_value = 1.0
  }
})
