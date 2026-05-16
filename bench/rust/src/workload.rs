use std::time::Instant;
use tokio::sync::mpsc;

// Per-item work mirroring the Tin/Go/Crystal workload: a few string
// allocations + an integer hash-mix so the body resists no-op
// elimination by the optimizer.
fn process_one(v: i64) -> i64 {
    let key = format!("item-{v}");
    let header = format!("[req] {key}");
    let trailer = format!("{key} :ok");
    let combo = format!("{header} | {trailer}");
    let m = combo.len() as i64;
    ((v.wrapping_mul(1315423911)) ^ (m.wrapping_mul(2654435761))) & 0x7fffffff
}

#[tokio::main(flavor = "current_thread")]
async fn main() {
    const N: u64 = 200_000;
    const WORKERS: usize = 8;

    let (in_tx, in_rx) = mpsc::channel::<i64>(64);
    let (out_tx, mut out_rx) = mpsc::channel::<i64>(64);

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
                    Some(v) => out_tx.send(process_one(v)).await.unwrap(),
                    None => return,
                }
            }
        });
    }
    drop(out_tx);

    let start = Instant::now();

    // Producer task drains independently from the sink loop below so a
    // full out-buffer doesn't deadlock the in-buffer.
    let producer = {
        let in_tx = in_tx.clone();
        tokio::spawn(async move {
            for i in 0..N {
                in_tx.send(i as i64).await.unwrap();
            }
        })
    };

    let mut acc: i64 = 0;
    for _ in 0..N {
        acc += out_rx.recv().await.unwrap();
    }
    producer.await.unwrap();

    let elapsed = start.elapsed();

    for _ in 0..WORKERS {
        in_tx.send(-1).await.unwrap();
    }

    println!("{N} items, {WORKERS} workers, acc={acc}");
    println!("elapsed: ~{}ms", elapsed.as_millis());
    println!("throughput: ~{} items/sec", (N as f64 / elapsed.as_secs_f64()) as u64);
}
