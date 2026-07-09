import statistics

nums = [42, 17, 99, 8, 73, 55, 31, 64]
print("analyze.py — my local code, running inside a crucible sandbox")
print(f"n={len(nums)}  mean={statistics.mean(nums):.1f}  median={statistics.median(nums)}  stdev={statistics.pstdev(nums):.2f}")
print("no image build, no Dockerfile")
