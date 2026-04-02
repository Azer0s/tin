N = 1_000_000
W = 8

tasks   = Channel(Int64).new(256)
results = Channel(Nil).new(256)

W.times do
  spawn do
    while (cost = tasks.receive?)
      cost.times { Fiber.yield }
      results.send(nil)
    end
  end
end

start = Time.monotonic

spawn do
  N.times { |i| tasks.send(i.to_i64 % 4) }
  tasks.close
end

N.times { results.receive }
elapsed = Time.monotonic - start

puts "#{N} tasks, #{W} workers, cost 0-3 yields/task"
puts "elapsed: ~#{elapsed.total_milliseconds.to_i}ms"
puts "throughput: ~#{(N / elapsed.total_seconds).to_i} tasks/sec"
