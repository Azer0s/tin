use std::time::Instant;
use tokio::sync::mpsc;

#[tokio::main(flavor = "current_thread")]
async fn main() {
    const N: u64 = 1_000_000;
    let (ping_tx, mut ping_rx) = mpsc::channel::<i64>(1);
    let (pong_tx, mut pong_rx) = mpsc::channel::<i64>(1);

    tokio::spawn(async move {
        while let Some(v) = ping_rx.recv().await {
            pong_tx.send(v).await.unwrap();
        }
    });

    let start = Instant::now();
    for _ in 0..N {
        ping_tx.send(1).await.unwrap();
        pong_rx.recv().await.unwrap();
    }
    let elapsed = start.elapsed();

    println!("{} round trips", N);
    println!("elapsed: ~{}ms", elapsed.as_millis());
    println!("latency: ~{}ns / round trip", elapsed.as_nanos() / N as u128);
}
