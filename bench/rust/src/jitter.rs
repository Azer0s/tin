use std::sync::Arc;
use std::time::Instant;
use tokio::sync::{mpsc, Mutex};

#[tokio::main(flavor = "current_thread")]
async fn main() {
    const N: usize = 1_000_000;
    const W: usize = 8;

    let (tasks_tx, tasks_rx) = mpsc::channel::<i64>(256);
    let (res_tx, mut res_rx) = mpsc::channel::<()>(256);
    let shared_rx = Arc::new(Mutex::new(tasks_rx));

    // W workers
    for _ in 0..W {
        let rx = Arc::clone(&shared_rx);
        let rtx = res_tx.clone();
        tokio::spawn(async move {
            loop {
                let cost = { rx.lock().await.recv().await };
                match cost {
                    Some(c) => {
                        for _ in 0..c {
                            tokio::task::yield_now().await;
                        }
                        rtx.send(()).await.unwrap();
                    }
                    None => break,
                }
            }
        });
    }
    drop(res_tx);

    let start = Instant::now();

    // Dispatcher
    tokio::spawn(async move {
        for i in 0..N {
            tasks_tx.send((i % 4) as i64).await.unwrap();
        }
    });

    let mut count = 0;
    while res_rx.recv().await.is_some() {
        count += 1;
        if count == N {
            break;
        }
    }
    let elapsed = start.elapsed();

    println!("{N} tasks, {W} workers, cost 0-3 yields/task");
    println!("elapsed: ~{}ms", elapsed.as_millis());
    println!("throughput: ~{} tasks/sec", (N as f64 / elapsed.as_secs_f64()) as u64);
}
