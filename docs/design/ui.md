# Design: Web UI redesign

> **Status:** Approved. Covers a redesign of the portal's web UI: a dashboard
> built around a replication flow chart, a sidebar layout with a
> Proxmox-inspired theme, a reorganized plan page, pages for long-running
> operations, and a wizard for new plans. See [DR plans](dr-plans.md),
> [Replication](replication.md), [Test failover](test-failover.md),
> [Failover](failover.md), and [Failback](failback.md).

## 1. Summary

- The app opens on a **dashboard**. Its centerpiece is a **flow chart**:
  primary hosts on the left, DR hosts on the right, each host listing its
  guests, with edges showing where every guest replicates. Edges pulse while
  data is actually moving.
- A collapsible **left sidebar** replaces the top navigation. The theme
  follows **Proxmox's colors** (orange accent, charcoal neutrals), with
  color-coded statuses, **dark mode**, and a wider, denser layout.
- The **plan page** stays a single page, with a sticky section menu,
  collapsible settings, a sticky save bar, and regrouped actions. New plans
  are created with a **wizard**.
- Long-running operations (takeover, test failover, failover, failback) get
  their own **operation pages** and a **history** per plan.
- Backend changes are limited to what the screens need: faster replication
  reports while a transfer runs, an overview RPC, and list RPCs for
  operation history (section 8).

## 2. Layout and theme

### 2.1 Navigation

A collapsible sidebar (shadcn `sidebar`) with grouped items:

- **Overview** (the dashboard, `/`)
- **DR plans**, **Hosts**
- **Alerts** (with a count of open alerts), **Audit log**
- **Settings**

The signed-in user, the theme toggle, and Sign out sit in the sidebar's
footer. Collapsed, the sidebar shows icons only. On narrow screens it opens
as a sheet.

Enrollment tokens leave the navigation: they're managed from the Hosts page
(the Add host dialog, plus a Tokens tab or section listing and revoking
them). `/tokens` redirects there.

### 2.2 Colors

- **Accent:** Proxmox orange (about `#E57000`) for primary buttons, the
  active navigation item, focus rings, and the flow chart's highlights.
  Neutrals are charcoal grays, close to Proxmox's own UI.
- **Statuses** use fixed semantic colors, kept apart from the accent:
  - green: healthy, online, running, passed
  - yellow: warning, RPO approaching the threshold, pending changes
  - red: failed, offline, RPO exceeded, errors
  - blue: in progress (replicating, test running, failing over or back)
  - gray: inactive, draft, paused, standby

  Warnings use a clear yellow rather than amber so they aren't mistaken for
  the orange accent. Every status also has an icon or label, never color
  alone.
- **Dark mode:** light, dark, or system, remembered in the browser. Both
  themes are defined through the existing CSS variables in `index.css`.
- Before building the pages, the theme is shown on two or three screens in
  both modes for approval.

### 2.3 Density

Pages use more of wide screens (a wider maximum width, or full width for the
dashboard and tables). Tables get tighter rows, right-aligned numbers,
monospace only for dataset names and addresses, and empty states that say
what to do next.

## 3. Dashboard

### 3.1 Flow chart

- **Columns:** hosts that are a primary in at least one plan on the left,
  hosts that are a DR host on the right. A host in both roles appears in
  each column it plays, with a note. Hosts in no plan appear in a small
  "Not in a plan" group below.
- **Host nodes** show the host's name, online status, and its guests (VMID,
  name, type). A host with many guests collapses to the first few plus
  "and N more", expandable.
- **Guests appear on both sides** of their plan: the copy on the primary
  and the replica on the DR host.
  - The copy that is **running** has a **green outline**.
  - The **standby** copy (the replica, or the stopped primary copy while
    failed over) has a **gray outline**.
  - After a failover the outlines swap: the DR copy is green, the primary
    copy gray (and marked locked).
  - Test clones don't appear as guests; a running test shows as a badge on
    the DR host's copy ("test running").
- Guests in no plan appear on their host without an edge, in two groups:
  - **Unconfigured:** not decided yet, with a dashed yellow outline. Plans
    on the host warn about them.
  - **Unprotected:** the user chose not to protect them (dimmed). Plans
    don't warn about them.

  The choice is recorded per host (who and when) and made or undone in a
  plan's Guests section, where each guest in no plan shows "Mark
  unprotected" or "Undo".
- **Edges** run from each guest's copy on the primary to its copy on the
  DR host, one per guest, with an **arrow** showing the direction the plan
  copies data (toward the DR host; back toward the primary while failing
  back).
  - Color follows the plan's health (green, yellow, red; gray when paused
    or inactive); each plan's section header, on both hosts, shows its
    status (RPO, lagging, failed over, and so on).
  - A guest's edge **pulses in the direction data moves** while its disks
    are being copied: replication (primary to DR) or a failback copy (DR
    to primary). During a failover, the plan's edges are blue and
    animated.
  - While a transfer runs, the section headers show progress (percent of
    the expected bytes).
- **Hover** on a guest shows its state and plan. Hovering its edge shows a
  card at once, next to the pointer: direction, plan status, when the
  guest was last replicated (the oldest of its disks' newest snapshots),
  the snapshot, copy progress, the alert threshold, and errors.
- **Click** a host to open its page, a plan name or guest to open the plan.
- The chart uses **React Flow** (`@xyflow/react`) with a computed layout;
  pan, zoom, and dragging are off. Animations respect
  `prefers-reduced-motion` (edges then show a static "transferring" style).

### 3.2 Around the chart

- **Summary tiles:** plans healthy out of total, worst RPO, hosts online out
  of total, open alerts. Each links to the relevant page.
- **Running operations:** any takeover, test failover, failover, or
  failback in progress, with its current step, linking to its operation
  page.
- **Getting started:** a checklist shown until every item is done (enroll a
  primary host, enroll a DR host, create a plan, activate it, run a test
  failover). It can be dismissed.

The dashboard polls like other pages do (`use-poll`), faster while a
transfer or operation is running.

## 4. Plan page

The page stays a single page, reorganized:

- **Header:** plan name, state and health badges, primary to DR hosts, and
  the actions (section 4.1).
- **Sticky section menu** on the side: Status, Tests, History, then the
  settings sections (General, Guests, Mappings, Network, Schedule and
  retention, Startup order, DNS records, Advanced). It highlights the
  section in view and jumps on click. Sections with validation errors are
  marked. The Hosts and Validation panels move under it.
- **Collapsible settings:** each settings card collapses to a one-line
  summary (for example "Every 5 minutes, alert after 15 minutes, DR keeps
  1h/24h/14d") and expands to edit. Cards with errors or unsaved edits
  start expanded.
- **Sticky save bar:** with unsaved edits, a bar at the bottom shows
  "Unsaved changes" with Discard and Save. On an active plan it explains
  that saved edits are pending until applied, as today.
- **Status:** replication health for active plans; for other states, what
  the plan is doing (paused, failing over or back, failed over: since
  when, the final snapshot, guests running at the DR site).
- **History:** a table of the plan's operations (takeover, tests,
  failovers, failbacks) with when, who, result, and a link to each
  operation's page. It replaces the separate test history table (built
  with the operation pages, section 5).
- Validation issues name their section, so the menu and the cards mark
  where each problem is, and choosing an issue opens its section.
- Stale copy is fixed (the DNS section's "comes with failover (phase 6)").

### 4.1 Actions

- **Primary buttons** depend on the state: for example Activate (draft),
  Apply changes (pending changes), Start test (active), Fail back (failed
  over).
- **Routine actions** (Pause, Resume, Deactivate, Discard changes, Delete)
  go in a "More" menu.
- **Fail over** stands apart: destructive styling, separated from the
  other buttons, and its dialog asks to type the plan's name before
  starting a real failover.

## 5. Operations

- Starting an operation still uses a dialog for options, preflight, and
  confirmation. Once started, the app goes to the **operation page**:
  - `/plans/:id/takeover`
  - `/plans/:id/tests/:testId`
  - `/plans/:id/failovers/:failoverId`
  - `/plans/:id/failbacks/:failbackId`
- An operation page shows the steps with their state and timing, progress
  (copy rounds, bytes), messages and errors, and the actions that apply
  (confirm, abort, retry, extend or end a test, set a verdict). It stays
  available after the operation finishes, as a record.
- The same content as today's dialogs, moved into pages; the dialogs keep
  only the start flow. Dialogs lose their duplicate Close button.
- `/plans/:id/failovers/latest` and `/failbacks/latest` show the plan's
  newest one, so status buttons link without knowing IDs. Opening the
  failover or failback dialog while one hasn't finished opens its page.
- The plan's test failover section shows the running test (linking to
  its page) or the last one; all operations are in the History section.

## 6. New-plan wizard

`/plans/new` becomes a wizard:

1. **Hosts:** name, primary host, DR host (with readiness hints).
2. **Guests:** which guests to protect (or adopt an existing zrepl setup).
3. **Mappings:** storage, networks, test bridge.
4. **Replication:** network (existing or EZDR tunnel), schedule, RPO alert,
   retention.
5. **Recovery:** startup order, DNS records (optional).
6. **Review:** summary with validation; create as a draft, or create and
   activate.

It reuses the plan page's section components, pre-filled from
`SuggestPlan`. Editing an existing plan stays on the plan page.

A step can be left once its sections have no errors (validation issues
name their section); the step's errors and warnings show under its cards.
The review shows each section collapsed to its summary, and choosing one
goes back to its step. "Create and activate" creates the plan and opens
the plan page with the usual activation (or takeover) preview.

## 7. Other pages

- **Hosts:** a denser table with the status icon's color, role (primary,
  DR host, both, or none), plans, guests, and last seen, and an Enrollment
  tokens tab (`/tokens` redirects to it). Host detail keeps its tabs; its
  guests show whether they're protected (with the plan), unconfigured
  ("Mark unprotected"), or unprotected ("Undo"), so guests of hosts in no
  plan can be marked too.
- **Alerts:** Firing / Resolved / All, severity, links to the plan and
  host, and how long resolved alerts lasted.
- **Audit log:** filters by kind (operations, plans, hosts and guests,
  sign-ins, settings), a search, and older events on request.
- **Settings:** unchanged apart from clearer wording.
- **Sign-in and setup:** restyled with the new theme.

## 8. Backend changes

- **Faster replication reports:** a client reports its zrepl status every
  few seconds (about 5) while any of its jobs is replicating, and once a
  minute otherwise (as today). The failback transfer reports its progress
  the same way. The portal already stores the latest report; no schema
  change.
- **`GetOverview`:** one RPC returning what the dashboard needs: hosts
  (status, role), their guests (from the inventory) with which plan
  protects each and where it's running, each plan's state, health, RPO,
  and current transfer progress, running operations, and counts. This
  avoids one inventory and status request per host and plan.
- **Operation history:** `ListFailovers` and `ListFailbacks` per plan (the
  records are already kept). Takeovers keep one record per plan, returned
  by `GetTakeover` as today.

## 9. Development

- A **dev portal with fake data**: a seed step creates a SQLite database
  with fake hosts, inventories, and plans in many states (draft, active and
  healthy, RPO exceeded, paused, failed over, failback running, test
  running). A small simulator updates replication reports so the flow
  chart's animations can be seen without real hosts. The lab isn't touched.
- The Vite dev server proxies `/ezdr.*` API paths to the dev portal
  (replacing the unused `/api` proxy).

## 10. Stages

1. Dev portal, seed data, and simulator; the sidebar layout, theme, and
   dark mode (theme approval before going further).
2. Dashboard: `GetOverview`, faster reports, the flow chart, tiles,
   running operations, getting started.
3. Plan page: section menu, collapsible settings, save bar, actions.
4. Operation pages and history.
5. New-plan wizard.
6. Other pages (Hosts with tokens, Alerts, Audit, Settings, sign-in).

Each stage is verified against the dev portal and, where it matters, the
lab, with screenshots for review.

## 11. Out of scope

- Pan, zoom, and rearranging the flow chart; actions from the chart.
- Tabs or separate sub-pages for plans.
- Mobile-first layouts (the UI stays usable on narrow screens, but targets
  desktops).
- Live push updates (the UI keeps polling).
