N       = 200_000
WORKERS = 8

# Per-item work mirroring the Tin/Go/Rust workload: a few string
# allocations + an integer hash-mix so the body can't be optimized to
# a no-op.
def process_one(v : Int64) : Int64
  key = "item-#{v}"
  header = "[req] " + key
  trailer = key + " :ok"
  combo = header + " | " + trailer
  m = combo.bytesize.to_i64

  ((v * 1315423911) ^ (m * 2654435761)) & 0x7fffffff
end

g_in  = Channel(Int64).new(64)
g_out = Channel(Int64).new(64)

WORKERS.times do
  spawn do
    loop do
      v = g_in.receive
      break if v < 0
      g_out.send(process_one(v))
    end
  end
end

start = Time.monotonic

# Producer fiber so the main fiber can drain g_out concurrently
# without deadlocking on a full out-buffer.
spawn do
  N.times { |i| g_in.send(i.to_i64) }
end

acc = 0_i64
N.times { acc += g_out.receive }
elapsed = Time.monotonic - start

WORKERS.times { g_in.send(-1_i64) }

puts "#{N} items, #{WORKERS} workers, acc=#{acc}"
puts "elapsed: ~#{elapsed.total_milliseconds.to_i}ms"
puts "throughput: ~#{(N / elapsed.total_seconds).to_i} items/sec"
