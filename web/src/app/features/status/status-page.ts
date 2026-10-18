import { ChangeDetectionStrategy, Component, DestroyRef, inject, signal } from '@angular/core';
import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { Health } from '../../core/health/health';
import { HealthService } from '../../core/health/health.service';

type LoadState = 'loading' | 'ok' | 'error';

@Component({
  selector: 'app-status-page',
  templateUrl: './status-page.html',
  styleUrl: './status-page.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class StatusPage {
  private readonly healthService = inject(HealthService);
  private readonly destroyRef = inject(DestroyRef);

  protected readonly state = signal<LoadState>('loading');
  protected readonly health = signal<Health | null>(null);

  constructor() {
    this.healthService
      .get()
      .pipe(takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: (health) => {
          this.health.set(health);
          this.state.set('ok');
        },
        error: () => {
          this.health.set(null);
          this.state.set('error');
        },
      });
  }
}
