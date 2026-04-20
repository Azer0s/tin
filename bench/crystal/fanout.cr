N       = 1_000_000
WORKERS = 8

g_in  = Channel(Int64).new(64)
g_out = Channel(Int64).new(64)

WORKERS.times do
  spawn do
    loop do
      v = g_in.receive
      break if v < 0
      g_out.send(v * 2)
    end
  end
end

start = Time.monotonic
N.times do |i|
  g_in.send(i.to_i64)
  g_out.receive
end
elapsed = Time.monotonic - start

WORKERS.times { g_in.send(-1_i64) }

puts "#{N} items, #{WORKERS} workers"
puts "elapsed: ~#{elapsed.total_milliseconds.to_i}ms"
puts "throughput: ~#{(N / elapsed.total_seconds).to_i} items/sec"
