// The departures board: split-flap tiles, one tile per character.
// Rows are real buttons so Tab reaches every voyage and Enter opens its ticket.
import { el, fmtBytes, siEl, STATE_SI } from "./util.js";
import { icon, flash, prefersReduced } from "./ui.js";
const COLS = ["ticket", "vessel", "berth", "cls", "hold", "status", "time"];
// Columns the header can sort. The board keeps the broker's queue order
// until the operator picks one.
const SORTABLE = { vessel: "vessel", hold: "hold", status: "status", time: "time" };
const STATUS_RANK = { waiting: 0, active: 1, parking: 2, parked: 3, draining: 4, releasing: 5, stuck: 6 };
function holdBytes(entry) {
  const l = entry.lease;
  if (entry.isWaiting) return l.needs > 0 ? l.needs : -1;
  if (l.state === "parked") return entry.fp > 0 ? entry.fp : -1; // the RAM its warm copy holds
  if (l.used > 0) return l.used;
  return l.needs > 0 ? l.needs : -1;
}
function sortEntries(entries, sort) {
  if (!sort || !SORTABLE[sort]) return entries;
  const arr = [...entries];
  const stateOf = (e) => (e.isWaiting ? "waiting" : e.lease.state);
  const ageOf = (e) => (e.lease.age_seconds == null ? -1 : e.lease.age_seconds);
  switch (sort) {
    case "vessel": arr.sort((a, b) => String(a.lease.engine || "").localeCompare(String(b.lease.engine || ""))); break;
    case "hold": arr.sort((a, b) => holdBytes(b) - holdBytes(a)); break;
    case "status": arr.sort((a, b) => (STATUS_RANK[stateOf(a)] ?? 9) - (STATUS_RANK[stateOf(b)] ?? 9)); break;
    case "time": arr.sort((a, b) => ageOf(b) - ageOf(a)); break;
  }
  return arr;
}

// Characters per column. Every cell always shows exactly this many tiles
// (padded with blank tiles, trimmed with "~"), so the board never wraps.
const WIDTH = { ticket: 4, vessel: 14, berth: 4, cls: 8, hold: 6, status: 11, time: 6 };
const CLASS = { foreground: "FIRST", interactive: "CABIN", batch: "STEERAGE" };

function fit(text, n) {
  const t = String(text).toUpperCase();
  return t.length > n ? t.slice(0, n - 1) + "~" : t.padEnd(n, " ");
}

// The ticket number is the lease's sequence; the full id is on the ticket.
function ticketNo(id) {
  const m = /(\d+)$/.exec(id || "");
  return m ? m[1].slice(-4).padStart(4, "0") : "----";
}

function gib(bytes) {
  return (Math.round(bytes / 1073741824 * 10) / 10).toFixed(1) + "G";
}
const HEADS = {
  ticket: "TICKET",
  vessel: "VESSEL",
  berth: "BERTH",
  cls: "CLASS",
  hold: "HOLD",
  status: "STATUS",
  time: "TIME",
};
const STAGGER_MS = 15;
function makeHead(onSort) {
  const h = el("div", "board-cols board-head");
  h._sortBtns = {};
  for (const c of COLS) {
    const cell = el("div", "cell");
    if (SORTABLE[c] && onSort) {
      const b = el("button", "bhead-btn");
      b.type = "button";
      b.dataset.sort = c;
      b.setAttribute("aria-pressed", "false");
      b.setAttribute("aria-label", "sort departures by " + HEADS[c].toLowerCase());
      b.append(el("span", null, HEADS[c]), icon("swap", 10));
      b.addEventListener("click", () => onSort(c));
      cell.append(b);
      h._sortBtns[c] = b;
    } else {
      cell.append(el("span", null, HEADS[c]));
    }
    h.append(cell);
  }
  return h;
}
function makeRow(key, onOpen) {
  const btn = el("button", "board-row");
  btn.type = "button";
  btn.dataset.key = key;
  btn._cells = {};
  for (const c of COLS) {
    const cell = el("div", c === "ticket" ? "cell lease-id-cell" : "cell");
    cell.dataset.col = c;
    // the phone's card variant has no column head, so each cell carries its own
    // label and CSS paints it
    cell.dataset.label = HEADS[c];
    cell._tiles = [];
    if (c === "status") {
      // Every status word leads with its shape, so the state reads without
      // colour. Tiles append after it, so it stays the cell's first child.
      cell._si = siEl(STATE_SI.active);
      cell.append(cell._si);
    }
    btn.append(cell);
    btn._cells[c] = cell;
  }
  btn.addEventListener("click", () => onOpen(key));
  return btn;
}
function flipTile(tile, ch, delay) {
  if (ch === tile.textContent) return;
  if (prefersReduced()) {
    tile.classList.remove("flipping");
    tile.textContent = ch;
    return;
  }
  // The character lands at once and the flap animation is only the visual:
  // the board must read correctly even where timers are throttled
  // (background tabs) or animations never run.
  tile.textContent = ch;
  tile.style.animationDelay = delay + "ms";
  tile.classList.remove("flipping");
  void tile.offsetWidth; // restart the animation
  tile.classList.add("flipping");
  if (!tile._endBound) {
    tile.addEventListener("animationend", () => tile.classList.remove("flipping"));
    tile._endBound = true;
  }
}
// the state glyph a status cell leads with: the five words are the vocabulary,
// so a state never looks one way on the board and another in the log
function syncSi(cell, st) {
  const name = STATE_SI[st];
  if (!cell || !cell._si || !name) return;
  const cls = "si si-" + name;
  if (cell._si.className !== cls) cell._si.className = cls;
}

// Reconcile a cell's tiles with new text. tone: "" | cyan | amber | magenta | dim
function updateCell(cell, text, tone) {
  const chars = Array.from(String(text));
  let tiles = cell._tiles;
  while (tiles.length < chars.length) {
    const t = el("span", "tile");
    t.textContent = "";
    cell.append(t);
    tiles = cell._tiles;
    tiles.push(t);
  }
  while (tiles.length > chars.length) {
    const t = tiles.pop();
    cell._tiles = tiles;
    t.remove();
  }
  cell._tiles = tiles;
  const toneCls = tone ? " tone-" + tone : "";
  chars.forEach((ch, i) => {
    const tile = tiles[i];
    const cls = ch === " " ? "tile space" : "tile" + toneCls;
    if (tile.className !== cls) tile.className = cls;
    flipTile(tile, ch, i * STAGGER_MS);
  });
}
export class Board {
  constructor(root, onOpenTicket, onSort) {
    this.root = root;
    this.onOpen = onOpenTicket;
    this.onSort = onSort || null;
    this.head = makeHead(this.onSort);
    this.rows = new Map(); // key -> button
    this.prevState = new Map(); // key -> status word, for the change flash
    this.empty = el("div", "board-empty", "No voyages. All berths free.");
    /* the rows sit in their own box: on a phone the board stops being a
       horizontal scroller, and the foot (the count and the order, the board's
       one announcement) stays outside the rows where it is always readable */
    this.rowsEl = el("div", "board-rows");
    this.root.append(this.head, this.rowsEl, this.empty);
    // the foot is the board's one announcement: the voyage count and the
    // order, not 200 flapping tiles inside a live region
    this.foot = el("div", "board-foot");
    this.foot.setAttribute("role", "status");
    this.root.append(this.foot);
  }
  focusRow(key) {
    const row = this.rows.get(key);
    if (row) row.focus();
  }
  // data: [{ key, lease, isWaiting }] in display order; sort names a
  // SORTABLE column or is null for the broker's own queue order
  render(data, berth, sort) {
    const ordered = sortEntries(data, sort);
    for (const c of Object.keys(this.head._sortBtns)) {
      const on = sort === c;
      this.head._sortBtns[c].classList.toggle("sorted", on);
      this.head._sortBtns[c].setAttribute("aria-pressed", String(on));
    }
    const seen = new Set();
    let i = 0;
    for (const entry of ordered) {
      seen.add(entry.key);
      let row = this.rows.get(entry.key);
      const isNew = !row;
      if (isNew) {
        row = makeRow(entry.key, this.onOpen);
        this.rows.set(entry.key, row);
      }
      const l = entry.lease;
      const st = entry.isWaiting ? "waiting" : l.state;
      // a VFIO hold (group "vfio") is a VM session holding the card, not an
      // engine's berth: the status word says so
      const word = !entry.isWaiting && l.group === "vfio" && st === "active" ? "IN VM" : statusText(st);
      if (!isNew && this.prevState.get(entry.key) !== word) flash(row, "row-changed");
      this.prevState.set(entry.key, word);
      const tone = { active: "cyan", draining: "amber", stuck: "magenta", waiting: "amber", parking: "amber", parked: "green" }[st] || "dim";
      const c = row._cells;
      // HOLD: what the driver measures once docked, what was asked for while
      // waiting, and what the engine's warm copy holds in RAM while parked.
      const hold = !entry.isWaiting && st === "parked" && entry.fp > 0 ? gib(entry.fp)
        : !entry.isWaiting && l.used > 0 ? gib(l.used)
        : l.needs > 0 ? gib(l.needs) : "CARD";
      updateCell(c.ticket, fit(ticketNo(l.id), WIDTH.ticket), "");
      updateCell(c.vessel, fit(l.engine || "?", WIDTH.vessel), "");
      updateCell(c.berth, fit(berth, WIDTH.berth), "");
      updateCell(c.cls, fit(CLASS[l.priority] || l.priority || "", WIDTH.cls), "");
      updateCell(c.hold, fit(hold, WIDTH.hold), "");
      // status cell: flip tiles, then the WARM and CONTESTED flags
      syncSi(c.status, st);
      updateCell(c.status, fit(word, WIDTH.status), tone);
      syncWarm(row, entry);
      syncQueued(row, entry);
      syncContest(c.status, l.contested);
      updateCell(c.time, fit(ageText(l.age_seconds), WIDTH.time), "");
      row.setAttribute("aria-label", "open ticket " + l.id + ", " + l.engine + ", " + word.toLowerCase());
      const title = rowTitle(entry);
      if (title) row.title = title;
      else row.removeAttribute("title");
      this.rowsEl.append(row); // keeps DOM order = display order
      i += 1;
    }
    for (const [key, row] of this.rows) {
      if (!seen.has(key)) {
        row.remove();
        this.rows.delete(key);
        this.prevState.delete(key);
      }
    }
    const hasRows = seen.size > 0;
    this.empty.hidden = hasRows;
    this.head.hidden = !hasRows;
    this.foot.hidden = !hasRows;
    this.foot.textContent = hasRows
      ? seen.size + (seen.size === 1 ? " voyage" : " voyages") + (sort && SORTABLE[sort] ? ", ordered by " + HEADS[sort].toLowerCase() : ", broker order")
      : "";
    return i;
  }
}
function statusText(st) {
  switch (st) {
    case "waiting": return "BOARDING";
    case "active": return "DOCKED";
    case "draining": return "CASTING OFF";
    case "parking": return "PARKING";
    case "parked": return "WARM";
    case "releasing": return "ADRIFT";
    case "stuck": return "AGROUND";
    default: return String(st || "").toUpperCase();
  }
}
function ageText(sec) {
  if (sec == null || !Number.isFinite(sec)) return "-";
  const s = Math.max(0, Math.round(sec));
  if (s < 60) return s + "S";
  const m = Math.floor(s / 60);
  if (m < 60) return m + "M" + String(s % 60).padStart(2, "0") + "S";
  const h = Math.floor(m / 60);
  return h + "H" + String(m % 60).padStart(2, "0") + "M";
}
// The flag sits in the row's grid on top of the status column, not inside the
// status cell, which clips anything past its fixed tile width.
// The WARM flag: the tier the weights live in. A HOT line would sit on every
// docked row, so only the parked state carries one, and the berth panel and
// the ticket spell the tiers in words.
function syncWarm(row, entry) {
  let flag = row.querySelector(".flag-warm");
  const l = entry.lease;
  const show = !entry.isWaiting && l.state === "parked";
  if (show && !flag) {
    flag = el("span", "flag flag-warm", "WARM");
    row.append(flag);
  } else if (!show && flag) {
    flag.remove();
  }
  if (show) {
    const at = l.parked_since != null ? " for " + ageWord(l.parked_since) : "";
    flag.title = "capacity parked in RAM (est. " + (entry.fp > 0 ? "about " + gib(entry.fp) : "unknown")
      + ")" + at + "; resume with berth resume " + (l.engine || "?");
  }
}
// syncQueued marks a waiting row's place in line: Q and the position, with
// the node's completion estimate when it sent one. Nothing renders when the
// status payload has neither field.
function syncQueued(row, entry) {
  let flag = row.querySelector(".flag-queue");
  const l = entry.lease;
  const show = entry.isWaiting && l.position != null;
  if (show && !flag) {
    flag = el("span", "flag flag-queue", "Q" + l.position);
    row.append(flag);
  } else if (!show && flag) {
    flag.remove();
  }
  if (show && l.estimate_s != null) flag.title += ", est. " + ageWord(l.estimate_s) + " to a berth";
}
// rowTitle is the hover text that has no room in the fixed board cells:
// the park, the queue and the container, each only when the status payload
// says it.
function rowTitle(entry) {
  const l = entry.lease;
  const bits = [];
  if (l.group === "vfio") bits.push("this hold pins the card for a virtual machine session");
  if (l.contested > 0) bits.push("granted while holders without a lease kept " + fmtBytes(l.contested) + " on this card");
  if (!entry.isWaiting && l.state === "parked") {
    const ram = entry.fp > 0 ? "about " + gib(entry.fp) : "an unknown amount";
    bits.push("weights parked in RAM" + (l.parked_since != null ? " for " + ageWord(l.parked_since) : "") + ", holding " + ram);
  }
  if (entry.isWaiting && l.position != null) {
    bits.push("waiting at queue position " + l.position + (l.estimate_s != null ? ", est. " + ageWord(l.estimate_s) + " to a berth" : ""));
  }
  if (l.container_name) {
    bits.push("runs in container " + l.container_name + (l.container ? " (" + l.container + ")" : ""));
  }
  return bits.join("; ");
}
// ageWord reads a seconds figure the way the log does: 6m 2s, 1h 30m.
function ageWord(sec) {
  if (sec == null || !Number.isFinite(sec)) return "";
  const s = Math.max(0, Math.round(sec));
  if (s < 60) return s + "s";
  const m = Math.floor(s / 60);
  if (m < 60) return m + "m " + (s % 60) + "s";
  const h = Math.floor(m / 60);
  return h + "h " + (m % 60) + "m";
}
function syncContest(statusCell, contested) {
  const row = statusCell.parentElement;
  let flag = row.querySelector(".flag-contest");
  const show = contested != null && contested > 0;
  if (show && !flag) {
    flag = el("span", "flag flag-contest", "CONTESTED");
    row.append(flag);
  } else if (!show && flag) {
    flag.remove();
  }
  if (show) flag.title = "granted while holders without a lease kept " + fmtBytes(contested) + " on this card";
}
