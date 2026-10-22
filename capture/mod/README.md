Records your map over time for [TimeShadeDB](https://github.com/Danielv123/TimeShadeDB): the map chart, trains, vehicles, players, chart tags, machine recipes and optionally rails. You can then scroll back through the map's history in a web viewer.

The mod writes the capture to `script-output/timeshade/`. Run `timeshadedb tail` next to the game to send it to a `timeshadedb serve` server:

```
timeshadedb serve --db mymap
timeshadedb tail --dir <factorio user data>/script-output/timeshade --map mymap
```

The map settings turn capture on or off and set the chart debounce, chart reads per tick, rail capture and extra forces. `/timeshade stats` prints counters and `/timeshade resync` resends everything.

In multiplayer only the server writes files. For Clusterio clusters use the TimeShadeDB Clusterio plugin instead of this mod.
