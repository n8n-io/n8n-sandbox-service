import { test, expect } from '@playwright/test';
import { apiRequest } from './helpers';

test.describe('Images API', () => {
  test('list images returns empty array', async ({ request }) => {
    const resp = await apiRequest(request, 'GET', '/images');
    expect(resp.status).toBe(200);
    const images = await resp.json();
    expect(Array.isArray(images)).toBe(true);
  });

  test('get non-existent image returns 404', async ({ request }) => {
    const resp = await apiRequest(request, 'GET', '/images/00000000-0000-0000-0000-000000000000');
    expect(resp.status).toBe(404);
  });

  test('get image with invalid id returns 400', async ({ request }) => {
    const resp = await apiRequest(request, 'GET', '/images/invalid-id');
    expect(resp.status).toBe(400);
  });

  test('delete non-existent image returns 404', async ({ request }) => {
    const resp = await apiRequest(request, 'DELETE', '/images/00000000-0000-0000-0000-000000000000');
    expect(resp.status).toBe(404);
  });

  test('delete image with invalid id returns 400', async ({ request }) => {
    const resp = await apiRequest(request, 'DELETE', '/images/invalid-id');
    expect(resp.status).toBe(400);
  });
});