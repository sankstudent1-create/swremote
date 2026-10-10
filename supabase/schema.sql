-- SWRemote v3 — Phase 1: accounts & private device dashboard
-- Run this in the Supabase SQL editor after creating the free project.
-- Auth uses Supabase Auth (email/password); these tables hold the rest.

-- One row per human user (extends auth.users)
create table if not exists profiles (
  id uuid primary key references auth.users(id) on delete cascade,
  display_name text,
  avatar_url text,
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

-- Auto-create a profiles row for every new auth user (standard Supabase pattern).
-- Without this, inserts into devices/branding fail on the profiles foreign key.
create or replace function public.handle_new_user()
returns trigger
language plpgsql
security definer set search_path = public
as $$
begin
  insert into public.profiles (id, display_name)
  values (new.id, nullif(new.raw_user_meta_data->>'display_name',''))
  on conflict (id) do update set
    display_name = coalesce(excluded.display_name, public.profiles.display_name);
  return new;
end;
$$;

drop trigger if exists on_auth_user_created on auth.users;
create trigger on_auth_user_created
  after insert on auth.users
  for each row execute function public.handle_new_user();

-- backfill profiles for users who signed up before the trigger existed
insert into public.profiles (id)
select id from auth.users
on conflict (id) do nothing;

-- v6.0: avatar_url column for existing projects + avatars storage bucket
alter table public.profiles add column if not exists avatar_url text;

insert into storage.buckets (id, name, public)
values ('avatars', 'avatars', true)
on conflict (id) do nothing;

drop policy if exists "public read avatars" on storage.objects;
create policy "public read avatars" on storage.objects
  for select using (bucket_id = 'avatars');

drop policy if exists "users manage own avatar" on storage.objects;
create policy "users manage own avatar" on storage.objects
  for all using (bucket_id = 'avatars' and auth.uid()::text = (storage.foldername(name))[1])
  with check (bucket_id = 'avatars' and auth.uid()::text = (storage.foldername(name))[1]);

-- profiles: users can read all (for viewer identity), update own
drop policy if exists "profiles readable" on public.profiles;
create policy "profiles readable" on public.profiles for select using (true);
drop policy if exists "profiles self-update" on public.profiles;
create policy "profiles self-update" on public.profiles for update using (auth.uid() = id);
