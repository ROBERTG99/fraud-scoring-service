-- score_tx.lua: read point-in-time features strictly before as_of, then (optionally) add the transaction.
-- One script call = one atomic step: nothing interleaves between the read and the add.
-- KEYS[1]=fr:{acc}:tx  KEYS[2]=fr:{acc}:dev  KEYS[3]=fr:{acc}:mcc  KEYS[4]=fr:{acc}:cred  KEYS[5]=fr:{acc}:devfirst
-- ARGV: as_of_ms, event_id, amount_minor, device_id, mcc, add(0|1)
local as_of = tonumber(ARGV[1])
local excl = '(' .. ARGV[1]

local function window(key, w)
  return redis.call('ZCOUNT', key, as_of - w, excl)
end

local function sum_window(key, w)
  local members = redis.call('ZRANGEBYSCORE', key, as_of - w, excl)
  local total = 0
  for _, m in ipairs(members) do
    local sep = string.find(m, '|', 1, true)
    total = total + tonumber(string.sub(m, sep + 1))
  end
  return total, #members
end

local function score_or_neg(key, member)
  local s = redis.call('ZSCORE', key, member)
  if s then return tonumber(s) end
  return -1
end

local c10 = window(KEYS[1], 600000)
local c1h = window(KEYS[1], 3600000)
local sum1h = sum_window(KEYS[1], 3600000)
local sum24, c24 = sum_window(KEYS[1], 86400000)
local distinct = redis.call('ZCOUNT', KEYS[2], as_of - 86400000, excl)
local dev_first = score_or_neg(KEYS[5], ARGV[4])
local mcc = score_or_neg(KEYS[3], ARGV[5])
-- Credential fields hold up to 16 change times (apply_event.lua); take the latest strictly before as_of.
local function latest_before(list)
  local best = -1
  if list then
    for v in string.gmatch(list, '[^,]+') do
      local n = tonumber(v)
      if n < as_of and n > best then best = n end
    end
  end
  return best
end
local cred = redis.call('HMGET', KEYS[4], 'pwd_changed_ms', 'email_changed_ms')
local pwd = latest_before(cred[1])
local email = latest_before(cred[2])

if ARGV[6] == '1' then
  redis.call('ZADD', KEYS[1], as_of, ARGV[2] .. '|' .. ARGV[3])
  redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', '(' .. (as_of - 87000000))
  redis.call('PEXPIRE', KEYS[1], 90000000)
  redis.call('ZADD', KEYS[2], 'GT', as_of, ARGV[4])
  redis.call('ZADD', KEYS[5], 'LT', as_of, ARGV[4])
  local cut = '(' .. (as_of - 7776000000)
  local old = redis.call('ZRANGEBYSCORE', KEYS[2], '-inf', cut)
  if #old > 0 then
    redis.call('ZREM', KEYS[5], unpack(old))
    redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', cut)
  end
  redis.call('PEXPIRE', KEYS[2], 7862400000)
  redis.call('PEXPIRE', KEYS[5], 7862400000)
  redis.call('ZADD', KEYS[3], 'GT', as_of, ARGV[5])
  redis.call('ZREMRANGEBYSCORE', KEYS[3], '-inf', '(' .. (as_of - 2592000000))
  redis.call('PEXPIRE', KEYS[3], 2678400000)
end

return {c10, c1h, sum1h, c24, sum24, distinct, dev_first, mcc, pwd, email}
