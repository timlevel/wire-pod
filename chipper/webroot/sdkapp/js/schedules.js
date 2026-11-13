"use strict";
const el = id => document.getElementById(id);
let editing = "";
let schedules = [];
const actionNames = {
  drive_off_charger: "Leave charger",
  say_text: "Say something",
  play_animation: "Play animation",
  drive_on_charger: "Return to charger"
};
function message(text, error = false) {
  el("message").textContent = text;
  el("message").className = error ? "error" : "";
}
async function request(path, options = {}) {
  const response = await fetch(path, { ...options, headers: { "Content-Type": "application/json", ...options.headers } });
  if (!response.ok) throw new Error(await response.text());
  return response.status === 204 || response.status === 202 ? null : response.json();
}
function addAction(action = { type: "say_text", text: "Hello!" }) {
  const row = document.createElement("div"); row.className = "action";
  const type = document.createElement("select"); type.setAttribute("aria-label", "Action type");
  for (const [value, name] of Object.entries(actionNames)) type.add(new Option(name, value));
  type.value = action.type;
  const value = document.createElement("input"); value.value = action.text || action.animation || "";
  function update() {
    value.hidden = !["say_text", "play_animation"].includes(type.value);
    value.required = !value.hidden;
    value.maxLength = type.value === "say_text" ? 1000 : 200;
    value.placeholder = type.value === "say_text" ? "It is {hour} o'clock!" : "Animation name";
    value.setAttribute("aria-label", type.value === "say_text" ? "Speech text" : "Animation name");
  }
  type.addEventListener("change", update); update();
  const remove = document.createElement("button"); remove.type = "button"; remove.textContent = "Remove";
  remove.addEventListener("click", () => row.remove());
  row.append(type, value, remove); el("actions").append(row);
}
function preset() {
  el("cron").value = "0 * * * *";
  el("actions").replaceChildren();
  addAction({ type: "drive_off_charger" });
  addAction({ type: "say_text", text: "It is {hour} o'clock!" });
  addAction({ type: "drive_on_charger" });
}
function reset() {
  editing = ""; el("editorTitle").textContent = "New schedule";
  el("name").value = "Hourly announcement"; el("enabled").checked = true;
  el("timezone").value = Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  preset();
}
function edit(s) {
  editing = s.id; el("editorTitle").textContent = "Edit schedule";
  el("name").value = s.name; el("cron").value = s.cron;
  el("timezone").value = s.timezone; el("enabled").checked = s.enabled;
  if (![...el("robot").options].some(o => o.value === s.esn)) el("robot").add(new Option(s.esn, s.esn));
  el("robot").value = s.esn;
  el("actions").replaceChildren(); (s.actions || []).forEach(addAction);
  el("scheduleForm").scrollIntoView({ behavior: "smooth" });
}
function payload(s) {
  return { id: s.id, name: s.name, esn: s.esn, cron: s.cron, timezone: s.timezone, enabled: s.enabled, actions: s.actions };
}
function stamp(value, zone) {
  if (!value || value.startsWith("0001-")) return "—";
  try { return new Date(value).toLocaleString(undefined, { timeZone: zone }); }
  catch { return new Date(value).toLocaleString(); }
}
function button(label, action) {
  const b = document.createElement("button"); b.type = "button"; b.textContent = label;
  b.addEventListener("click", async () => {
    b.disabled = true;
    try { await action(); } catch (err) { message(err.message, true); }
    finally { b.disabled = false; }
  });
  return b;
}
async function load() {
  schedules = await request("/api-schedules/");
  el("schedules").replaceChildren();
  if (!schedules.length) el("schedules").textContent = "No schedules yet. Save the hourly announcement above to get started.";
  for (const s of schedules) {
    const card = document.createElement("article"); card.className = "card";
    const title = document.createElement("h3"); title.textContent = s.name;
    const status = document.createElement("p"); status.className = "badge";
    status.textContent = `${s.enabled ? "Enabled" : "Paused"}${s.running ? " · Running" : ""} · ${s.esn}`;
    const timing = document.createElement("p"); timing.textContent = `${s.cron} · ${s.timezone}`;
    const actions = document.createElement("p");
    actions.textContent = (s.actions || []).map(a => (actionNames[a.type] || a.type) + (a.text ? `: “${a.text}”` : a.animation ? `: ${a.animation}` : "")).join(" → ");
    const next = document.createElement("p"); next.className = "muted";
    next.textContent = `Next: ${stamp(s.nextRun, s.timezone)} · Last: ${stamp(s.lastRun, s.timezone)}`;
    const result = document.createElement("p");
    result.textContent = s.loadError ? `Saved schedule needs repair: ${s.loadError}` : s.lastError ? `Last error: ${s.lastError}` : s.running ? "Actions are running…" : stamp(s.lastRun, s.timezone) !== "—" ? "Last run completed." : "Has not run yet.";
    if (s.loadError) result.className = "error";
    card.append(title, status, timing, actions, next, result,
      button("Edit", () => edit(s)),
      button(s.enabled ? "Pause" : "Enable", async () => {
        if (s.loadError) { edit(s); message("Fix this schedule and save before enabling it.", true); return; }
        await request("/api-schedules/", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ ...payload(s), enabled: !s.enabled }) }); await load();
      }),
      button("Run now", async () => {
        if (s.loadError) throw new Error("Fix this schedule and save before running it.");
        await request(`/api-schedules/${s.id}/run`, { method: "POST" }); message("Run started. Status updates below."); await load();
      }),
      button("Delete", async () => {
        if (!window.confirm(`Delete “${s.name}”?`)) return;
        await request(`/api-schedules/${s.id}`, { method: "DELETE" });
        if (editing === s.id) reset(); await load();
      }));
    el("schedules").append(card);
  }
}
el("scheduleForm").addEventListener("submit", async event => {
  event.preventDefault();
  const actions = [...el("actions").children].map(row => {
    const type = row.querySelector("select").value, value = row.querySelector("input").value;
    return { type, ...(type === "say_text" ? { text: value } : type === "play_animation" ? { animation: value } : {}) };
  });
  const schedule = { id: editing, name: el("name").value, esn: el("robot").value, cron: el("cron").value, timezone: el("timezone").value, enabled: el("enabled").checked, actions };
  try {
    const saved = await request("/api-schedules/", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(schedule) });
    editing = saved.id; el("editorTitle").textContent = "Edit schedule";
    message("Schedule saved."); await load();
  } catch (err) { message(err.message, true); }
});
el("hourly").addEventListener("click", preset);
el("reset").addEventListener("click", reset);
el("addAction").addEventListener("click", () => addAction());
reset();
(async () => {
  try {
    const robots = await request("/api-schedule-robots");
    robots.forEach(serial => el("robot").add(new Option(serial, serial)));
    if (!robots.length) message("No authenticated robots. Add Vector in Bot Settings first.");
    await load();
  } catch (err) { message(err.message, true); }
})();
setInterval(() => load().catch(err => message(err.message, true)), 5000);
