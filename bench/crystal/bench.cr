N = 1_000_000

ping = Channel(Int64).new(1)
pong = Channel(Int64).new(1)

spawn do
  N.times { pong.send(ping.receive) }
end

start = Time.monotonic
N.times do
  ping.send(1_i64)
  pong.receive
end
elapsed = Time.monotonic - start

puts "#{N} round trips"
puts "elapsed: ~#{elapsed.total_milliseconds.to_i}ms"
puts "latency: ~#{(elapsed.total_nanoseconds / N).to_i}ns / round trip"
