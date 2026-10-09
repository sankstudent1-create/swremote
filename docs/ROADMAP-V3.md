# SWRemote — v3 Roadmap ("complete all")

> Approved by sanky 2026-10-09. Build order is dependency order — each phase
> stands on the previous one.

## Phase 0 — Performance core ✅ (this release, v3.0)

- [x] Tiled dirty-region streaming (25–30 fps target) — `docs/PERFORMANCE.md`
- [x] Native-res tiles (no more blurry downscale)
- [x] Remote cursor position overlay
- [x] In-depth docs (architecture, protocol, performance, this roadmap)

## Phase 1 — Accounts & private dashboard

**Goal**: sign in with user ID + password; your devices appear automatically;
nobody else can see them. ID+PIN stays as the "quick help" path.

- [ ] Supabase project (free tier): Auth (email/password) + Postgres
- [ ] Tables: `profiles`, `devices` (id, user_id, agent_id, name, claim_code,
      last_seen), `device_settings` (auto_start, quality…), `branding`
- [ ] Website: login/signup screens, session via Supabase JWT
- [ ] Relay: verify Supabase JWT on viewer `join`; `/api/devices` returns only
      the caller's devices
- [ ] Agent: claim flow — agent shows a 6-char claim code in its window; user
      enters it once in the dashboard → device linked to the account forever
- [ ] Same-account connect: no PIN needed (already authenticated on both ends)
- [ ] Schema: `supabase/schema.sql` (prepared — needs a Supabase project first)

**Needs from sanky**: create the free Supabase project, paste URL + anon key.

## Phase 2 — Branding & remote settings push

- [ ] Dashboard branding editor: business name, logo upload, accent color
- [ ] Agent fetches branding at startup → window shows *their* logo/name
      (runtime white-label — no per-customer builds)
- [ ] Dashboard → relay → agent `{"t":"push_settings",…}`: auto-start on boot,
      default quality, etc. Agent applies + confirms

## Phase 3 — Two-way communication & files

- [ ] Real chat **window on the PC side** (agent): incoming messages pop a
      proper window, agent user can reply → appears on viewer's sheet
- [ ] Remote **file explorer**: browse PC directories, download/upload/delete
      (same-account: full; ID+PIN guests: agent user approves each session)

## Phase 4 — Meeting mode & A/V (hardest — last)

- [ ] Meeting mode: multi-viewer session, voice
- [ ] Remote camera/mic capture in the Go agent (deep Windows API work —
      no good pure-Go libs; R&D phase, honest timeline TBD)
- [ ] **Non-negotiable**: always-visible on-screen indicator on the PC while
      camera/mic is accessed + explicit permission prompt per session

## Also queued

- [ ] P2P investigation (STUN/TURN) to cut relay latency
- [ ] DXGI Desktop Duplication capture (faster than GDI BitBlt; needs real
      Windows testing — can't be validated on the Linux build VM)
- [ ] Downloads section on the website (installer + exe)
