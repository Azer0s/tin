// MPMC via Arc<Mutex<Receiver>>: W producers + W consumers on one shared channel.
use std::sync::Arc;
use std::time::Instant;
use tokio::sync::{mpsc, Mutex};

#[tokio::main(flavor = "current_thread")]
async fn main() {
    const N: usize = 1_000_000;
    const W: usize = 4;

    let (tx, rx) = mpsc::channel::<i64>(64);
    let (done_tx, mut done_rx) = mpsc::channel::<()>(W);
    let shared_rx = Arc::new(Mutex::new(rx));

    // W consumers
    for _ in 0..W {
        let rx = Arc::clone(&shared_rx);
        let dtx = done_tx.clone();
        tokio::spawn(async move {
            for _ in 0..(N / W) {
                rx.lock().await.recv().await.unwrap();
            }
            dtx.send(()).await.unwrap();
        });
    }
    drop(done_tx);

    let start = Instant::now();

    // W producers
    for _ in 0..W {
        let ptx = tx.clone();
        tokio::spawn(async move {
            for j in 0..(N / W) {
                ptx.send(j as i64).await.unwrap();
            }
        });
    }
    drop(tx);

    while done_rx.recv().await.is_some() {}
    let elapsed = start.elapsed();

    println!("{N} msgs, {W}P+{W}C");
    println!("elapsed: ~{}ms", elapsed.as_millis());
    println!("throughput: ~{} msgs/sec", (N as f64 / elapsed.as_secs_f64()) as u64);
}
