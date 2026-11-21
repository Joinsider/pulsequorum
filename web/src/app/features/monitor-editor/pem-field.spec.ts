// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { ComponentFixture, TestBed } from '@angular/core/testing';
import { FormControl } from '@angular/forms';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { PEM_MAX_FILE_BYTES, PemField, isPemField } from './pem-field';

const PEM = '-----BEGIN CERTIFICATE-----\nMIIBszCCAVmgAwIBAgIUB\n-----END CERTIFICATE-----\n';

const EN = {
  monitorEditor: {
    pem: {
      loadFile: 'Load file…',
      tooLarge: 'The file is larger than 16 KiB.',
      notPem: 'Not a PEM file',
    },
  },
};

/** A File-like object: File.text() is the only part the component reads. */
function file(content: string, size = new TextEncoder().encode(content).length) {
  return { size, text: vi.fn(() => Promise.resolve(content)) };
}

/** A change event of the hidden file input, as the browser fires it. */
function changeEvent(picked: ReturnType<typeof file> | null): Event {
  return {
    target: { files: picked ? [picked] : [], value: 'C:\\fakepath\\cert.pem' },
  } as unknown as Event;
}

describe('isPemField', () => {
  it('matches long string properties named like certificates and keys', () => {
    expect(isPemField({ type: 'string', maxLength: 16384 }, 'clientCertificate.certificate')).toBe(
      true,
    );
    expect(isPemField({ type: 'string', maxLength: 16384 }, 'clientCertificate.privateKey')).toBe(
      true,
    );
  });

  it('does not match short strings or other long strings', () => {
    expect(isPemField({ type: 'string', maxLength: 256 }, 'certificate')).toBe(false);
    expect(isPemField({ type: 'string', maxLength: 65536 }, 'body')).toBe(false);
    expect(isPemField({ type: 'string' }, 'privateKey')).toBe(false);
  });
});

describe('PemField', () => {
  let fixture: ComponentFixture<PemField>;
  let control: FormControl<string>;

  beforeEach(() => {
    TestBed.configureTestingModule({
      imports: [
        PemField,
        TranslocoTestingModule.forRoot({
          langs: { en: EN },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
    });
    fixture = TestBed.createComponent(PemField);
    control = new FormControl<string>('', { nonNullable: true });
    fixture.componentRef.setInput('control', control);
    fixture.componentRef.setInput('label', 'Certificate (PEM)');
    fixture.detectChanges();
  });

  function fileError(): string | null {
    return fixture.componentInstance['fileError']();
  }

  it('renders a multi-line monospace textarea and the load button', () => {
    const el = fixture.nativeElement as HTMLElement;
    const textarea = el.querySelector('textarea');
    expect(textarea).not.toBeNull();
    expect(textarea?.classList.contains('mono')).toBe(true);
    expect(textarea?.getAttribute('rows')).toBe('6');
    expect(el.querySelector('input[type="file"]')?.getAttribute('accept')).toBe(
      '.pem,.crt,.cer,.key,.txt',
    );
    expect(el.querySelector('button')?.textContent?.trim()).toBe('Load file…');
  });

  it('reads a PEM file into the control', async () => {
    const picked = file(`${PEM}\n`);
    await fixture.componentInstance['onFile'](changeEvent(picked));

    expect(picked.text).toHaveBeenCalledOnce();
    expect(control.value).toBe(PEM.trim());
    expect(control.dirty).toBe(true);
    expect(fileError()).toBeNull();
  });

  it('accepts a file of exactly the size limit', async () => {
    const picked = file(PEM, PEM_MAX_FILE_BYTES);
    await fixture.componentInstance['onFile'](changeEvent(picked));

    expect(control.value).toBe(PEM.trim());
    expect(fileError()).toBeNull();
  });

  it('rejects a file above 16 KiB without reading it and keeps the value', async () => {
    control.setValue('kept');
    const picked = file(PEM, PEM_MAX_FILE_BYTES + 1);
    await fixture.componentInstance['onFile'](changeEvent(picked));

    expect(picked.text).not.toHaveBeenCalled();
    expect(control.value).toBe('kept');
    expect(fileError()).toBe('monitorEditor.pem.tooLarge');
  });

  it('rejects a file without a PEM block and keeps the value', async () => {
    control.setValue('kept');
    const picked = file('0\u0082\u0003\u0001binary DER data');
    await fixture.componentInstance['onFile'](changeEvent(picked));

    expect(control.value).toBe('kept');
    expect(fileError()).toBe('monitorEditor.pem.notPem');
  });

  it('shows the inline error for a rejected file', async () => {
    await fixture.componentInstance['onFile'](changeEvent(file('no pem here')));
    fixture.detectChanges();

    const alert = (fixture.nativeElement as HTMLElement).querySelector('[role="alert"]');
    expect(alert?.textContent?.trim()).toBe('Not a PEM file');
  });

  it('clears the file input and ignores a cancelled picker', async () => {
    const event = changeEvent(null);
    await fixture.componentInstance['onFile'](event);

    expect(control.value).toBe('');
    expect(fileError()).toBeNull();
    expect((event.target as HTMLInputElement).value).toBe('');
  });
});
