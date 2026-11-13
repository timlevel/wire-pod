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
// Common animations already used by wire-pod's conversational responses.
const animationChoices = {
  anim_onboarding_reacttoface_happy_01: "Happy",
  anim_blackjack_victorwin_01: "Very happy",
  anim_feedback_meanwords_01: "Sad",
  anim_rtpickup_loop_10: "Angry",
  anim_feedback_shutup_01: "Frustrated",
  anim_observing_self_absorbed_01: "Darting eyes",
  anim_meetvictor_lookface_timeout_01: "Confused",
  anim_explorer_scan_short_04: "Thinking",
  anim_pounce_success_03: "Celebrate",
  anim_feedback_iloveyou_02: "Love"
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
  const fields = document.createElement("div"); fields.className = "action-fields";
  const speech = document.createElement("input"); speech.className = "speech-text";
  speech.value = action.text || ""; speech.maxLength = 1000;
  speech.placeholder = "It is {hour} o'clock!"; speech.setAttribute("aria-label", "Speech text");
  const animationLabel = document.createElement("label"); animationLabel.textContent = "Animation";
  const animation = document.createElement("select"); animation.className = "animation-choice"; animation.setAttribute("aria-label", "Animation");
  animation.add(new Option("Choose an animation…", ""));
  for (const [name, label] of Object.entries(animationChoices)) animation.add(new Option(label, name));
  animation.add(new Option("Custom animation name…", "custom"));
  animation.value = action.animation ? (Object.hasOwn(animationChoices, action.animation) ? action.animation : "custom") : "";
  animationLabel.append(animation);
  const customLabel = document.createElement("label"); customLabel.textContent = "Custom animation name";
  const custom = document.createElement("input"); custom.className = "animation-custom";
  custom.value = animation.value === "custom" ? action.animation : ""; custom.maxLength = 200;
  custom.placeholder = "Exact animation name, e.g. anim_pounce_success_03";
  customLabel.append(custom);
  const help = document.createElement("small"); help.className = "muted";
  help.textContent = "Choose a common animation. Custom names must exactly match an animation on your Vector.";
  function update() {
    const isSpeech = type.value === "say_text", isAnimation = type.value === "play_animation";
    fields.hidden = !isSpeech && !isAnimation;
    speech.hidden = !isSpeech; speech.required = isSpeech;
    animationLabel.hidden = !isAnimation; animation.required = isAnimation;
    customLabel.hidden = !isAnimation || animation.value !== "custom";
    custom.required = !customLabel.hidden;
    help.hidden = !isAnimation;
  }
  type.addEventListener("change", update); animation.addEventListener("change", update); update();
  const remove = document.createElement("button"); remove.type = "button"; remove.textContent = "Remove";
  remove.addEventListener("click", () => row.remove());
  fields.append(speech, animationLabel, customLabel, help);
  row.append(type, fields, remove); el("actions").append(row);
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
    const type = row.querySelector("select").value;
    const animation = row.querySelector(".animation-choice").value;
    return { type, ...(type === "say_text" ? { text: row.querySelector(".speech-text").value } : type === "play_animation" ? { animation: animation === "custom" ? row.querySelector(".animation-custom").value : animation } : {}) };
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
