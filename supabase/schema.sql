-- SWRemote v3 — Phase 1: accounts & private device dashboard
-- Run this in the Supabase SQL editor after creating the free project.
-- Auth uses Supabase Auth (email/password); these tables hold the rest.

-- One row per human user (extends auth.users)
create table if not exists profiles (
  id uuid primary key references auth.users(id) on delete cascade,
  display_name text,
  created_at timestamptz default now()
);

-- Devices claimed to an account (via the agent's claim code)
create table if not exists devices (
  id uuid primary key default gen_random_uuid(),
  user_id uuid not null references profiles(id) on delete cascade,
  agent_id text not null unique,          -- the 9-digit device ID
  name text not null default 'My PC',
  claim_code text unique,                  -- 6-char code shown by the agent, nulled after claim
  claimed_at timestamptz,
  last_seen timestamptz,
  created_at timestamptz default now()
);
create index if not exists devices_user_idx on devices(user_id);

-- Per-device settings pushed from the dashboard
create table if not exists device_settings (
  device_id uuid primary key references devices(id) on delete cascade,
  auto_start boolean default false,
  quality int default 70,
  updated_at timestamptz default now()
);

-- White-label branding per account (Phase 2)
create table if not exists branding (
  user_id uuid primary key references profiles(id) on delete cascade,
  business_name text,
  logo_url text,
  accent_color text default '#2f7de1',
  updated_at timestamptz default now()
);

-- Row-level security: users only ever see their own rows
alter table profiles enable row level security;
alter table devices enable row level security;
alter table device_settings enable row level security;
alter table branding enable row level security;

create policy "own profile" on profiles
  for all using (auth.uid() = id);
create policy "own devices" on devices
  for all using (auth.uid() = user_id);
create policy "own device settings" on device_settings
  for all using (auth.uid() = (select user_id from devices where devices.id = device_settings.device_id));
create policy "own branding" on branding
  for all using (auth.uid() = user_id);
