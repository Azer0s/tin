N = 1_000_000
W = 4

ch   = Channel(Int64).new(64)
done = Channel(Nil).new(W)

W.times do
  spawn do
    (N // W).times { ch.receive }
    done.send(nil)
  end
end

start = Time.monotonic

W.times do
  spawn do
    (N // W).times { |j| ch.send(j.to_i64) }
  end
end

W.times { done.receive }
elapsed = Time.monotonic - start

puts "#{N} msgs, #{W}P+#{W}C"
puts "elapsed: ~#{elapsed.total_milliseconds.to_i}ms"
puts "throughput: ~#{(N / elapsed.total_seconds).to_i} msgs/sec"
