use std::time::Instant;
use tokio::sync::mpsc;

#[tokio::main(flavor = "current_thread")]
async fn main() {
    const N: u64 = 1_000_000;
    const WORKERS: usize = 8;

    let (in_tx, in_rx) = mpsc::channel::<i64>(64);
    let (out_tx, mut out_rx) = mpsc::channel::<i64>(64);

    // Wrap in_rx in Arc<Mutex> so workers can share it
    let in_rx = std::sync::Arc::new(tokio::sync::Mutex::new(in_rx));

    for _ in 0..WORKERS {
        let in_rx = in_rx.clone();
        let out_tx = out_tx.clone();
        tokio::spawn(async move {
            loop {
                let v = {
                    let mut rx = in_rx.lock().await;
                    rx.recv().await
                };
                match v {
                    Some(v) if v < 0 => return,
                    Some(v) => out_tx.send(v * 2).await.unwrap(),
                    None => return,
                }
            }
        });
    }
    drop(out_tx);

    let start = Instant::now();
    for i in 0..N {
        in_tx.send(i as i64).await.unwrap();
        out_rx.recv().await.unwrap();
    }
    let elapsed = start.elapsed();

    for _ in 0..WORKERS {
        in_tx.send(-1).await.unwrap();
    }

    println!("{N} items, {WORKERS} workers");
    println!("elapsed: ~{}ms", elapsed.as_millis());
    println!("throughput: ~{} items/sec", (N as f64 / elapsed.as_secs_f64()) as u64);
}
