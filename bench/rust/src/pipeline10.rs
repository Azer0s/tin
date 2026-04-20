use std::time::Instant;
use tokio::sync::mpsc;

#[tokio::main(flavor = "current_thread")]
async fn main() {
    const N: u64 = 500_000;
    const STAGES: usize = 10;

    let (tx0,  mut rx0)  = mpsc::channel::<i64>(1);
    let (tx1,  mut rx1)  = mpsc::channel::<i64>(1);
    let (tx2,  mut rx2)  = mpsc::channel::<i64>(1);
    let (tx3,  mut rx3)  = mpsc::channel::<i64>(1);
    let (tx4,  mut rx4)  = mpsc::channel::<i64>(1);
    let (tx5,  mut rx5)  = mpsc::channel::<i64>(1);
    let (tx6,  mut rx6)  = mpsc::channel::<i64>(1);
    let (tx7,  mut rx7)  = mpsc::channel::<i64>(1);
    let (tx8,  mut rx8)  = mpsc::channel::<i64>(1);
    let (tx9,  mut rx9)  = mpsc::channel::<i64>(1);
    let (tx10, mut rx10) = mpsc::channel::<i64>(1);

    tokio::spawn(async move { while let Some(v) = rx0.recv().await  { tx1.send(v + 1).await.unwrap(); } });
    tokio::spawn(async move { while let Some(v) = rx1.recv().await  { tx2.send(v + 1).await.unwrap(); } });
    tokio::spawn(async move { while let Some(v) = rx2.recv().await  { tx3.send(v + 1).await.unwrap(); } });
    tokio::spawn(async move { while let Some(v) = rx3.recv().await  { tx4.send(v + 1).await.unwrap(); } });
    tokio::spawn(async move { while let Some(v) = rx4.recv().await  { tx5.send(v + 1).await.unwrap(); } });
    tokio::spawn(async move { while let Some(v) = rx5.recv().await  { tx6.send(v + 1).await.unwrap(); } });
    tokio::spawn(async move { while let Some(v) = rx6.recv().await  { tx7.send(v + 1).await.unwrap(); } });
    tokio::spawn(async move { while let Some(v) = rx7.recv().await  { tx8.send(v + 1).await.unwrap(); } });
    tokio::spawn(async move { while let Some(v) = rx8.recv().await  { tx9.send(v + 1).await.unwrap(); } });
    tokio::spawn(async move { while let Some(v) = rx9.recv().await  { tx10.send(v + 1).await.unwrap(); } });

    let start = Instant::now();
    for i in 0..N {
        tx0.send(i as i64).await.unwrap();
        rx10.recv().await.unwrap();
    }
    let elapsed = start.elapsed();

    println!("{N} messages through {STAGES} stages");
    println!("elapsed: ~{}ms", elapsed.as_millis());
    println!("latency: ~{}ns / pipeline pass", elapsed.as_nanos() / N as u128);
}
