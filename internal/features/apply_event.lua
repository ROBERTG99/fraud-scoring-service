-- apply_event.lua: apply one login, device or transaction event (feature-updater). Idempotent:
-- re-applying the same event_id changes nothing (ZSET members are unique, ZADD GT and max() for creds).
-- KEYS[1]=fr:{acc}:tx  KEYS[2]=fr:{acc}:dev  KEYS[3]=fr:{acc}:mcc  KEYS[4]=fr:{acc}:cred
-- ARGV: kind(login|device|tx), ts_ms, event_id, amount_minor, device_id, mcc, change, success(0|1)
local kind = ARGV[1]
local ts = tonumber(ARGV[2])

local function sight_device()
  redis.call('ZADD', KEYS[2], 'GT', ts, ARGV[5])
  redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', '(' .. (ts - 7776000000))
  redis.call('PEXPIRE', KEYS[2], 7862400000)
end

if kind == 'tx' then
  redis.call('ZADD', KEYS[1], ts, ARGV[3] .. '|' .. ARGV[4])
  redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', '(' .. (ts - 87000000))
  redis.call('PEXPIRE', KEYS[1], 90000000)
  sight_device()
  redis.call('ZADD', KEYS[3], 'GT', ts, ARGV[6])
  redis.call('ZREMRANGEBYSCORE', KEYS[3], '-inf', '(' .. (ts - 2592000000))
  redis.call('PEXPIRE', KEYS[3], 2678400000)
elseif kind == 'login' then
  if ARGV[8] == '1' then sight_device() end
elseif kind == 'device' then
  local change = ARGV[7]
  if change == 'device_bound' then
    sight_device()
  elseif change == 'password_changed' or change == 'email_changed' then
    local field = 'pwd_changed_ms'
    if change == 'email_changed' then field = 'email_changed_ms' end
    local cur = tonumber(redis.call('HGET', KEYS[4], field)) or -1
    if ts > cur then redis.call('HSET', KEYS[4], field, ts) end
    redis.call('PEXPIRE', KEYS[4], 7862400000)
  end
end
return 1
