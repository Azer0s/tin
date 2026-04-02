N      = 1_000_000
STAGES = 4

channels = Array.new(STAGES + 1) { Channel(Int64).new(1) }

STAGES.times do |i|
  in_ch  = channels[i]
  out_ch = channels[i + 1]
  spawn { loop { out_ch.send(in_ch.receive + 1) } }
end

start = Time.monotonic
N.times { |i| channels[0].send(i.to_i64); channels[STAGES].receive }
elapsed = Time.monotonic - start

puts "#{N} messages through #{STAGES} stages"
puts "elapsed: ~#{elapsed.total_milliseconds.to_i}ms"
puts "latency: ~#{(elapsed.total_nanoseconds / N).to_i}ns / pipeline pass"
