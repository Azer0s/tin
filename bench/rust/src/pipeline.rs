use std::time::Instant;
use tokio::sync::mpsc;

#[tokio::main(flavor = "current_thread")]
async fn main() {
    const N: u64 = 1_000_000;
    const STAGES: usize = 4;

    let (tx0, mut rx0) = mpsc::channel::<i64>(1);
    let (tx1, mut rx1) = mpsc::channel::<i64>(1);
    let (tx2, mut rx2) = mpsc::channel::<i64>(1);
    let (tx3, mut rx3) = mpsc::channel::<i64>(1);
    let (tx4, mut rx4) = mpsc::channel::<i64>(1);

    tokio::spawn(async move { while let Some(v) = rx0.recv().await { tx1.send(v + 1).await.unwrap(); } });
    tokio::spawn(async move { while let Some(v) = rx1.recv().await { tx2.send(v + 1).await.unwrap(); } });
    tokio::spawn(async move { while let Some(v) = rx2.recv().await { tx3.send(v + 1).await.unwrap(); } });
    tokio::spawn(async move { while let Some(v) = rx3.recv().await { tx4.send(v + 1).await.unwrap(); } });

    let start = Instant::now();
    for i in 0..N {
        tx0.send(i as i64).await.unwrap();
        rx4.recv().await.unwrap();
    }
    let elapsed = start.elapsed();

    println!("{N} messages through {STAGES} stages");
    println!("elapsed: ~{}ms", elapsed.as_millis());
    println!("latency: ~{}ns / pipeline pass", elapsed.as_nanos() / N as u128);
}
