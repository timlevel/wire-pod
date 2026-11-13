# Scheduled Vector actions

Open **Schedules** from the wire-pod home page. Authenticate Vector in **Bot Settings** first; he does not need to be online to save a schedule.

The **hourly announcement** preset runs `0 * * * *`: leave the charger, say “It is {hour} o'clock!”, then return to the charger. Select Vector and a timezone, then save. The browser's timezone is the initial default; each schedule keeps its own timezone. No jobs are enabled until you save one.

Each schedule has an ordered sequence of up to 20 actions: leave charger, say text, play a named animation, or return to charger. Speech replaces `{hour}` with 1–12, `{hour24}` with 0–23, and `{time}` with a time such as `3:00 PM`. Placeholders use the run's start time in the schedule's timezone.

Cron has five fields: **minute hour day-of-month month weekday**. Lists, ranges, and steps are supported. Sunday is 0. Examples:

| Expression | Runs |
| --- | --- |
| `0 * * * *` | On the hour |
| `0 9-20 * * *` | Hourly from 9 AM through 8 PM |
| `30 8 * * 1-5` | Weekdays at 8:30 AM |
| `*/15 * * * *` | Every 15 minutes |

Timezone names are IANA names such as `America/Chicago` or `Europe/London`. Daylight-saving transitions follow cron wall-clock behavior: nonexistent local times are skipped; repeated local times may run twice. The timezone database is bundled for installations without system timezone files. Cron parsing uses [robfig/cron v3](https://pkg.go.dev/github.com/robfig/cron/v3).

**Run now** tests a saved schedule immediately, including paused schedules. Last-run status and errors update every five seconds. **Pause** prevents future scheduled starts; pausing, editing, or deleting a job does not interrupt an already running sequence. Returning to the charger requires Vector to see and reach it. Physical movement and docking need testing with your robot.

Wire-pod and Vector must be running and connected at the scheduled time. Jobs do not replay missed occurrences after restart or reconnect. Runs on the same robot never overlap or queue; a conflicting occurrence is skipped. SDK connections time out after five seconds, each action after forty seconds, and the entire sequence after two minutes. The runner waits for behavior control and releases it at the end. If a speech/animation action fails after undocking, it attempts the requested final return while time remains. Failures are reported instead of being treated as success.

Schedules are stored atomically in **`jdocs/schedules.json`**, alongside the existing persisted robot data. Packaged builds use their configured `jdocs` directory; Docker uses its existing data volume. A malformed saved file disables the scheduler and reports a server error without replacing the file. Last-run status is held in memory and resets at restart. Back up this file with the rest of wire-pod data.

## Local API

The scheduler uses the existing wire-pod web server and its network access model. It does not invoke shell commands or arbitrary scripts.

- `GET /api-schedule-robots` lists robot serials without SDK credentials.
- `GET /api-schedules/` lists saved schedules and runtime status.
- `POST /api-schedules/` creates or updates a schedule; omit `id` for creation.
- `DELETE /api-schedules/{id}` removes a schedule.
- `POST /api-schedules/{id}/run` starts a manual test (202); a busy robot returns 409.

Example request body:

```json
{
  "name": "Hourly announcement",
  "esn": "YOUR_VECTOR_SERIAL",
  "cron": "0 * * * *",
  "timezone": "America/Chicago",
  "enabled": true,
  "actions": [
    { "type": "drive_off_charger" },
    { "type": "say_text", "text": "It is {hour} o'clock!" },
    { "type": "drive_on_charger" }
  ]
}
```
